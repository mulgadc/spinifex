//go:build e2e

package harness

import (
	"fmt"
	"os/exec"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/network/host"
)

// DumpIMDSDatapathDiagnostics emits a triage bundle for an IMDS reachability
// failure on the per-tap datapath. Captures the br-imds bridge realisation (its
// ports + demux/forward flows), the OVN guest port-binding reached via the
// br-imds<->br-int patch, the per-tap reply routing, the root-netns responder
// listener, conntrack, and br-int flows for the guest.
// Non-fatal — best-effort diagnostics; skips if OVS tooling or sudo unavailable.
func DumpIMDSDatapathDiagnostics(t *testing.T, subnetID, guestIP, artifactDir string) {
	t.Helper()
	fmt.Printf("\n%s%s── DIAGNOSTICS: IMDS datapath (subnet=%s guest=%s) ──%s\n",
		colorBold, colorCyan, subnetID, guestIP, colorReset)
	defer fmt.Printf("%s%s── END DIAGNOSTICS ──%s\n\n", colorBold, colorCyan, colorReset)

	if _, err := exec.LookPath("ovs-ofctl"); err != nil {
		fmt.Printf("  ovs-ofctl unavailable (%v); skipping IMDS datapath dump\n", err)
		return
	}
	if err := exec.Command("sudo", "-n", "true").Run(); err != nil {
		fmt.Printf("  passwordless sudo unavailable (%v); skipping IMDS datapath dump\n", err)
		return
	}

	const imdsIP = "169.254.169.254"

	runHostCaptures(t, artifactDir, []hostCapture{
		{
			filename: "imds-brimds-ports.txt",
			label:    "ovs-vsctl list-ports br-imds (primary tap + endpoint + patch present?)",
			argv:     []string{"ovs-vsctl", "list-ports", host.IMDSBridge},
		},
		{
			filename: "imds-brimds-flows.txt",
			label:    "ovs-ofctl dump-flows br-imds (demux priority=200 + forward priority=100)",
			argv:     []string{"ovs-ofctl", "dump-flows", host.IMDSBridge},
		},
		{
			filename: "imds-portbinding.txt",
			label:    "ovn-sbctl Port_Binding for the guest IP's LSP (bound via the br-int patch end?)",
			argv: []string{"ovn-sbctl", "--bare", "--columns=logical_port,chassis,up,mac",
				"list", "Port_Binding"},
			grepFor: []string{guestIP},
		},
		{
			filename: "imds-reply-rules.txt",
			label:    "ip rule (per-tap 'oif <endpoint> lookup <table>' reply rule present?)",
			argv:     []string{"ip", "rule"},
			grepFor:  []string{"ime-"},
		},
		{
			filename: "imds-reply-routes.txt",
			label:    "ip route show table all (per-tap default dev <endpoint>)",
			argv:     []string{"ip", "route", "show", "table", "all"},
			grepFor:  []string{"ime-"},
		},
		{
			filename: "imds-listener.txt",
			label:    "ss -ltnp (per-tap responder bound to 169.254.169.254:80 in root netns)",
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
			filename: "imds-brint-flows.txt",
			label:    "ovs-ofctl dump-flows br-int (guest + IMDS, filtered)",
			argv:     []string{"ovs-ofctl", "dump-flows", "br-int"},
			grepFor:  []string{imdsIP, guestIP},
		},
	})
}
