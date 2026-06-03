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
//   - Host realisation: the imds-h-<short> veth's address/route/neighbour state
//     and the listener socket — the load-bearing question is whether the host
//     has any L3 path back to guestIP (the SO_BINDTODEVICE reply path), since a
//     veth with no address/route can accept the SYN but never return the
//     SYN-ACK.
//   - In-flight evidence: conntrack for 169.254.169.254 (a stuck SYN_RECV is the
//     smoking gun that the host received the SYN and replied but the ACK never
//     came back) and the br-int flows touching the IMDS / guest addresses.
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
	vpcRouter := "vpc-" + vpcID
	const imdsIP = "169.254.169.254"

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
			filename: "imds-host-veth-addr.txt",
			label:    "ip -d addr show imds-h (host veth — expect NO address)",
			argv:     []string{"ip", "-d", "addr", "show", "dev", hostEnd},
		},
		{
			filename: "imds-host-route-to-guest.txt",
			label:    "ip route get <guest> oif imds-h (reply-path route?)",
			argv:     []string{"ip", "route", "get", guestIP, "oif", hostEnd},
		},
		{
			filename: "imds-host-neigh.txt",
			label:    "ip neigh show dev imds-h (host ARP cache)",
			argv:     []string{"ip", "neigh", "show", "dev", hostEnd},
		},
		{
			filename: "imds-listener.txt",
			label:    "ss -ltnp sport :80 (IMDS listener bound?)",
			argv:     []string{"ss", "-ltnp"},
			grepFor:  []string{imdsIP, ":80"},
		},
		{
			filename: "imds-conntrack.txt",
			label:    "conntrack -L (169.254.169.254 — SYN_RECV = reply lost)",
			argv:     []string{"conntrack", "-L"},
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
