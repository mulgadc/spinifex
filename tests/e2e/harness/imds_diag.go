//go:build e2e

package harness

import (
	"fmt"
	"os/exec"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/network/host"
)

// DumpIMDSDatapathDiagnostics emits a triage bundle for an IMDS reachability
// failure (guest curl to 169.254.169.254 times out). It captures both halves of
// the per-VPC datapath so a triager can tell request-path from reply-path:
//
//   - OVN realisation: is imds-port-<vpc> bound/up on this chassis, is the
//     169.254.169.254/32 static route installed on the VPC LR, is the localport
//     LSP defined as expected.
//   - Host realisation (inside the per-VPC netns imds-<short>): the imds-h-<short>
//     veth's address/route/neighbour state and the listener socket — the
//     load-bearing question is whether the netns has a real L3 path back to
//     guestIP. After the per-VPC-netns fix the veth carries 169.254.169.254/30
//     with a default route via the .253 LRP, so a healthy dump shows the IPv4
//     address, a neighbour entry for .253, and a reply route via .253; the
//     pre-fix failure showed an addressless veth that accepted the SYN but never
//     returned the SYN-ACK.
//   - In-flight evidence: conntrack (per-netns) for 169.254.169.254 (a stuck
//     SYN_RECV is the smoking gun that the host received the SYN and replied but
//     the ACK never came back) and the br-int flows touching the IMDS / guest
//     addresses.
//
// Non-fatal — runs purely for log/artifact signal so the test's own Fatal still
// wins. Skips silently when OVN tooling / passwordless sudo aren't available, so
// it's safe to call from developer laptops.
func DumpIMDSDatapathDiagnostics(t *testing.T, vpcID, guestIP, artifactDir string) {
	t.Helper()
	fmt.Printf("\n%s%s── DIAGNOSTICS: IMDS datapath (vpc=%s guest=%s) ──%s\n",
		colorBold, colorCyan, vpcID, guestIP, colorReset)
	defer fmt.Printf("%s%s── END DIAGNOSTICS ──%s\n\n", colorBold, colorCyan, colorReset)

	if _, err := exec.LookPath("ovn-nbctl"); err != nil {
		fmt.Printf("  ovn-nbctl unavailable (%v); skipping IMDS datapath dump\n", err)
		return
	}
	if err := exec.Command("sudo", "-n", "true").Run(); err != nil {
		fmt.Printf("  passwordless sudo unavailable (%v); skipping IMDS datapath dump\n", err)
		return
	}

	imdsPort := "imds-port-" + vpcID
	hostEnd := host.IMDSHostVethName(vpcID)
	ovsEnd := host.IMDSOVSPortName(vpcID)
	netns := host.IMDSNetnsName(vpcID)
	vpcRouter := "vpc-" + vpcID
	const imdsIP = "169.254.169.254"

	// nsExec wraps an argv to run inside the per-VPC netns, where the host veth
	// end and the IMDS listener now live.
	nsExec := func(argv ...string) []string {
		return append([]string{"ip", "netns", "exec", netns}, argv...)
	}

	runHostCaptures(t, artifactDir, []hostCapture{
		{
			filename: "imds-portbinding.txt",
			label:    "ovn-sbctl Port_Binding imds-port (chassis,up)",
			argv: []string{"ovn-sbctl", "--bare", "--columns=logical_port,chassis,up,mac",
				"find", "Port_Binding", "logical_port=" + imdsPort},
		},
		{
			filename: "imds-lsp-nb.txt",
			label:    "ovn-nbctl imds-port LSP (type,addresses,options)",
			argv: []string{"ovn-nbctl", "--bare", "--columns=name,type,addresses,options",
				"find", "Logical_Switch_Port", "name=" + imdsPort},
		},
		{
			filename: "imds-lr-routes.txt",
			label:    "ovn-nbctl lr-route-list (169.254.169.254/32 static route?)",
			argv:     []string{"ovn-nbctl", "lr-route-list", vpcRouter},
			grepFor:  []string{imdsIP},
		},
		{
			filename: "imds-netns.txt",
			label:    "ip netns list (per-VPC IMDS netns present?)",
			argv:     []string{"ip", "netns", "list"},
			grepFor:  []string{netns},
		},
		{
			filename: "imds-host-veth-addr.txt",
			label:    "ip -d addr show imds-h in netns (expect 169.254.169.254/30)",
			argv:     nsExec("ip", "-d", "addr", "show", "dev", hostEnd),
		},
		{
			filename: "imds-host-route-to-guest.txt",
			label:    "ip route get <guest> in netns (expect via 169.254.169.253)",
			argv:     nsExec("ip", "route", "get", guestIP),
		},
		{
			filename: "imds-host-neigh.txt",
			label:    "ip neigh show in netns (expect .253 LRP entry)",
			argv:     nsExec("ip", "neigh", "show"),
		},
		{
			filename: "imds-listener.txt",
			label:    "ss -ltnp in netns (IMDS listener bound?)",
			argv:     nsExec("ss", "-ltnp"),
			grepFor:  []string{imdsIP, ":80"},
		},
		{
			filename: "imds-conntrack.txt",
			label:    "conntrack -L in netns (169.254.169.254 — SYN_RECV = reply lost)",
			argv:     nsExec("conntrack", "-L"),
			grepFor:  []string{imdsIP},
		},
		{
			filename: "imds-ovs-flows.txt",
			label:    "ovs-ofctl dump-flows br-int (IMDS + guest, filtered)",
			argv:     []string{"ovs-ofctl", "dump-flows", "br-int"},
			grepFor:  []string{imdsIP, guestIP},
		},
		{
			filename: "imds-ovs-iface.txt",
			label:    "ovs-vsctl Interface imds-o (iface-id binding, ofport)",
			argv: []string{"ovs-vsctl", "--columns=name,external_ids,ofport",
				"list", "Interface", ovsEnd},
		},
	})
}
