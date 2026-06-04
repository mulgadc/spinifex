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
// the per-subnet L2 datapath so a triager can tell request-path from reply-path:
//
//   - OVN realisation: is imds-port-<subnet> bound/up on this chassis, is the
//     localport LSP defined as expected. There is no VPC-LR static route to check
//     under L2 — the localport sits directly on the subnet switch, so the request
//     never enters a router.
//   - Host realisation (inside the per-subnet netns imds-<short>): the
//     imds-h-<short> veth's address/route/neighbour state and the listener socket
//     — the load-bearing question is whether the netns has a real L2 path back to
//     guestIP. The veth carries 169.254.169.254/30 plus the subnet CIDR on-link, so
//     a healthy dump shows the IPv4 address, an on-link reply route out the veth
//     (no .253 gateway hop), and a resolved neighbour entry for guestIP; a broken
//     dump shows an addressless veth that accepted the SYN but never returned the
//     SYN-ACK.
//   - In-flight evidence: conntrack (per-netns) for 169.254.169.254 (a stuck
//     SYN_RECV is the smoking gun that the host received the SYN and replied but
//     the ACK never came back) and the br-int flows touching the IMDS / guest
//     addresses.
//
// Non-fatal — runs purely for log/artifact signal so the test's own Fatal still
// wins. Skips silently when OVN tooling / passwordless sudo aren't available, so
// it's safe to call from developer laptops.
func DumpIMDSDatapathDiagnostics(t *testing.T, subnetID, guestIP, artifactDir string) {
	t.Helper()
	fmt.Printf("\n%s%s── DIAGNOSTICS: IMDS datapath (subnet=%s guest=%s) ──%s\n",
		colorBold, colorCyan, subnetID, guestIP, colorReset)
	defer fmt.Printf("%s%s── END DIAGNOSTICS ──%s\n\n", colorBold, colorCyan, colorReset)

	if _, err := exec.LookPath("ovn-nbctl"); err != nil {
		fmt.Printf("  ovn-nbctl unavailable (%v); skipping IMDS datapath dump\n", err)
		return
	}
	if err := exec.Command("sudo", "-n", "true").Run(); err != nil {
		fmt.Printf("  passwordless sudo unavailable (%v); skipping IMDS datapath dump\n", err)
		return
	}

	imdsPort := "imds-port-" + subnetID
	subnetSwitch := "subnet-" + subnetID
	hostEnd := host.IMDSHostVethName(subnetID)
	ovsEnd := host.IMDSOVSPortName(subnetID)
	netns := host.IMDSNetnsName(subnetID)
	const imdsIP = "169.254.169.254"

	// nsExec wraps an argv to run inside the per-subnet netns, where the host veth
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
			filename: "imds-subnet-ports.txt",
			label:    "ovn-nbctl lsp-list subnet switch (localport present on the guest's switch?)",
			argv:     []string{"ovn-nbctl", "lsp-list", subnetSwitch},
			grepFor:  []string{imdsPort},
		},
		{
			filename: "imds-netns.txt",
			label:    "ip netns list (per-subnet IMDS netns present?)",
			argv:     []string{"ip", "netns", "list"},
			grepFor:  []string{netns},
		},
		{
			filename: "imds-host-veth-addr.txt",
			label:    "ip -d addr show imds-h in netns (expect 169.254.169.254/30 + subnet CIDR on-link)",
			argv:     nsExec("ip", "-d", "addr", "show", "dev", hostEnd),
		},
		{
			filename: "imds-host-route-to-guest.txt",
			label:    "ip route get <guest> in netns (expect on-link dev imds-h, no gateway hop)",
			argv:     nsExec("ip", "route", "get", guestIP),
		},
		{
			filename: "imds-host-neigh.txt",
			label:    "ip neigh show in netns (expect a resolved entry for the guest IP)",
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
