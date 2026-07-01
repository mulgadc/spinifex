//go:build e2e

package harness

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
)

// VPCDiagnosticsOpts carries the optional context dumpVPCFlowDiagnostics
// uses to target IP-specific captures (OF flows, conntrack, ARP, NAT
// lookup). Empty fields mean "skip the captures that need this input"
// — the unconditional captures (console-output, ovn-nbctl/sbctl show)
// still run.
type VPCDiagnosticsOpts struct {
	// ExternalIP is the public IP the test was probing (e.g. the
	// dnat_and_snat external address). When set, the helper greps
	// br-int flows / conntrack / host ARP for this IP.
	ExternalIP string
	// LogicalIP is the VM's private IP. When set, the OF-flow grep
	// also matches this address so we see both halves of the NAT
	// translation.
	LogicalIP string
	// ArtifactDir is the per-test bundle directory (typically
	// `fix.Artifacts`). When set, every capture is also persisted as
	// a separate file so the Stage 2 analyzer bundler picks it up
	// alongside the journal + test-log slices.
	ArtifactDir string
}

// DumpVPCFlowDiagnostics emits a triage bundle for VPC/IGW datapath failures.
// Dumps the guest console tail and a slim OVN state snapshot to identify
// whether the gateway router has converged. Non-fatal — log signal only.
// Skips silently if OVN tooling / passwordless sudo aren't available.
func DumpVPCFlowDiagnostics(t *testing.T, c *AWSClient, instanceID, label string, opts VPCDiagnosticsOpts) {
	t.Helper()
	fmt.Printf("\n%s%s── DIAGNOSTICS: %s (instance=%s) ──%s\n",
		colorBold, colorCyan, label, instanceID, colorReset)

	dumpConsoleOutput(t, c, instanceID)
	dumpOVNState(t, instanceID)
	dumpDatapathState(t, opts)

	fmt.Printf("%s%s── END DIAGNOSTICS ──%s\n\n", colorBold, colorCyan, colorReset)
}

func dumpConsoleOutput(t *testing.T, c *AWSClient, instanceID string) {
	t.Helper()
	out, err := c.EC2.GetConsoleOutput(&ec2.GetConsoleOutputInput{
		InstanceId: aws.String(instanceID),
	})
	if err != nil {
		fmt.Printf("  console-output: GetConsoleOutput failed: %v\n", err)
		return
	}
	if out.Output == nil || aws.StringValue(out.Output) == "" {
		fmt.Println("  console-output: empty")
		return
	}
	raw, derr := base64.StdEncoding.DecodeString(aws.StringValue(out.Output))
	if derr != nil {
		raw = []byte(aws.StringValue(out.Output))
	}
	tail := raw
	if len(tail) > 4096 {
		tail = tail[len(tail)-4096:]
	}
	fmt.Printf("  console-output (last %d bytes):\n", len(tail))
	for _, line := range strings.Split(strings.TrimRight(string(tail), "\n"), "\n") {
		fmt.Printf("    | %s\n", line)
	}
}

// DumpInstanceConsole writes an instance's full serial console (base64-decoded)
// to <dir>/<name>. The lb-agent logs its data-plane engine activation and any
// `reload nginx:` error to the guest console, which the host daemon journal
// never sees — capturing it makes an NLB health-check timeout diagnosable from
// CI artifacts alone. Best-effort: failures log, they don't fail the test.
func DumpInstanceConsole(t *testing.T, c *AWSClient, instanceID, dir, name string) {
	t.Helper()
	out, err := c.EC2.GetConsoleOutput(&ec2.GetConsoleOutputInput{
		InstanceId: aws.String(instanceID),
	})
	if err != nil {
		t.Logf("console capture: GetConsoleOutput(%s) failed: %v", instanceID, err)
		return
	}
	encoded := aws.StringValue(out.Output)
	if encoded == "" {
		t.Logf("console capture: %s empty", instanceID)
		return
	}
	raw, derr := base64.StdEncoding.DecodeString(encoded)
	if derr != nil {
		raw = []byte(encoded)
	}
	DumpFile(t, dir, name, raw)
}

func dumpOVNState(t *testing.T, instanceID string) {
	t.Helper()
	if _, err := exec.LookPath("ovn-nbctl"); err != nil {
		fmt.Printf("  ovn-nbctl unavailable (%v); skipping OVN dump\n", err)
		return
	}
	if err := exec.Command("sudo", "-n", "true").Run(); err != nil {
		fmt.Printf("  passwordless sudo unavailable (%v); skipping OVN dump\n", err)
		return
	}

	cmds := []struct {
		label string
		args  []string
	}{
		{"ovn-nbctl show", []string{"ovn-nbctl", "show"}},
		{"ovn-sbctl show", []string{"ovn-sbctl", "show"}},
		{"Logical_Router external_ids", []string{"ovn-nbctl", "--bare",
			"--columns=name,external_ids", "list", "Logical_Router"}},
		{"NAT rules", []string{"ovn-nbctl", "list", "NAT"}},
		{"Port_Binding (chassis,up)", []string{"ovn-sbctl", "--bare",
			"--columns=logical_port,chassis,up", "list", "Port_Binding"}},
	}

	for _, c := range cmds {
		fmt.Printf("  --- %s ---\n", c.label)
		var stdout, stderr bytes.Buffer
		cmd := exec.Command("sudo", append([]string{"-n"}, c.args...)...)
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		cmd.Env = append(cmd.Env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
		if err := runWithTimeout(cmd, 5*time.Second); err != nil {
			fmt.Printf("    (failed: %v; stderr=%q)\n", err, stderr.String())
			continue
		}
		for _, line := range strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n") {
			if line == "" {
				continue
			}
			fmt.Printf("    %s\n", line)
		}
	}
}

// dumpDatapathState captures OVS / kernel + OVN control state keyed on the
// external IP: OF flows, conntrack, upstream ARP, plus the gateway router's
// per-stage logical-flow packet counts, MAC_Binding, static routes and the
// VM's logical port — enough to locate a pre-SNAT egress drop. Each capture
// is also written to opts.ArtifactDir when set. Skipped silently when inputs
// or shell tools are unavailable.
func dumpDatapathState(t *testing.T, opts VPCDiagnosticsOpts) {
	t.Helper()
	if opts.ExternalIP == "" {
		return
	}
	if err := exec.Command("sudo", "-n", "true").Run(); err != nil {
		fmt.Printf("  datapath: passwordless sudo unavailable (%v); skipping captures\n", err)
		return
	}

	runHostCaptures(t, opts.ArtifactDir, []hostCapture{
		{
			filename: "ovs-flows-extip.txt",
			label:    "ovs-ofctl dump-flows br-int (filtered)",
			argv:     []string{"ovs-ofctl", "dump-flows", "br-int"},
			grepFor:  []string{opts.ExternalIP, opts.LogicalIP},
		},
		{
			// br-int flows miss the localnet egress hop; br-ext is where
			// SNAT'd traffic leaves for the physical uplink, so a dropped
			// or absent flow here pinpoints an egress black-hole.
			filename: "ovs-flows-brext.txt",
			label:    "ovs-ofctl dump-flows br-ext (egress localnet)",
			argv:     []string{"ovs-ofctl", "dump-flows", "br-ext"},
		},
		{
			filename: "conntrack-extip.txt",
			label:    "conntrack -L (filtered)",
			argv:     []string{"conntrack", "-L"},
			grepFor:  []string{opts.ExternalIP},
		},
		{
			filename: "ip-neigh-extip.txt",
			label:    "ip neigh show (host ARP, filtered)",
			argv:     []string{"ip", "-4", "neigh", "show"},
			grepFor:  []string{opts.ExternalIP},
		},
		{
			filename: "ovn-nat-extip.txt",
			label:    "ovn-nbctl find NAT external_ip",
			argv: []string{"ovn-nbctl", "--bare", "--columns=_uuid,type,external_ip,logical_ip,external_mac,logical_port",
				"find", "NAT", "external_ip=" + opts.ExternalIP},
		},
		{
			// Per-stage packet counts across the whole gateway pipeline. The
			// SNAT flow reading n_packets=0 while the guest transmits means
			// packets die upstream of lr_out_snat; this grep shows which
			// stage (lr_in_ip_routing, lr_in_arp_resolve, ...) still counts
			// the packets and which one drops to zero — the exact drop point.
			filename: "ovn-lflows-extip.txt",
			label:    "ovn-sbctl --stats lflow-list (filtered)",
			argv:     []string{"ovn-sbctl", "--stats", "lflow-list"},
			grepFor:  []string{opts.ExternalIP, opts.LogicalIP},
		},
		{
			// The default route's next-hop must resolve to a MAC before the
			// router can deliver egress. An empty / stale MAC_Binding for the
			// uplink next-hop strands the packet in lr_in_arp_resolve, exactly
			// the pre-SNAT drop we are chasing.
			filename: "ovn-mac-binding.txt",
			label:    "ovn-sbctl list MAC_Binding (next-hop ARP)",
			argv:     []string{"ovn-sbctl", "list", "MAC_Binding"},
		},
		{
			// Is 0.0.0.0/0 → <uplink next-hop> actually programmed, and does
			// its output_port match the gateway router port? A missing or
			// mis-pointed default route drops egress in lr_in_ip_routing.
			filename: "ovn-lr-static-routes.txt",
			label:    "ovn-nbctl list Logical_Router_Static_Route",
			argv:     []string{"ovn-nbctl", "list", "Logical_Router_Static_Route"},
		},
		{
			// Maps the VM's private IP to its logical switch port (addresses
			// column holds "MAC IP") and shows up-state; a down or missing
			// port would blackhole egress before it reaches the gateway
			// router. Complements the chassis view in dumpOVNState.
			filename: "ovn-lsp-logical.txt",
			label:    "ovn-nbctl list Logical_Switch_Port (VM port)",
			argv: []string{"ovn-nbctl", "--bare",
				"--columns=name,addresses,up,type", "list", "Logical_Switch_Port"},
			grepFor: []string{opts.LogicalIP},
		},
	})
}

// hostCapture is one `sudo`-run diagnostic command. grepFor, when non-empty,
// post-filters stdout to lines containing any of the substrings (keeps
// ovs-ofctl / conntrack output tight without an external pipe).
type hostCapture struct {
	filename string
	label    string
	argv     []string
	grepFor  []string
}

// runHostCaptures runs each capture under `sudo -n`, echoes it to stdout for the
// CI log, and persists it as a separate file under artifactDir (when set) so the
// Stage 2 analyzer bundler picks it up. Assumes the caller already verified
// passwordless sudo is available.
func runHostCaptures(t *testing.T, artifactDir string, caps []hostCapture) {
	t.Helper()
	for _, cap := range caps {
		fmt.Printf("  --- %s ---\n", cap.label)
		var stdout, stderr bytes.Buffer
		cmd := exec.Command("sudo", append([]string{"-n"}, cap.argv...)...)
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		cmd.Env = append(cmd.Env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
		err := runWithTimeout(cmd, 5*time.Second)
		body := stdout.String()
		if len(cap.grepFor) > 0 {
			body = filterLines(body, cap.grepFor)
		}
		if err != nil {
			body = fmt.Sprintf("(capture failed: %v; stderr=%q)\n%s", err, stderr.String(), body)
		} else if strings.TrimSpace(body) == "" {
			body = "(no matches)\n"
		}
		for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
			fmt.Printf("    %s\n", line)
		}
		if artifactDir != "" {
			path := filepath.Join(artifactDir, cap.filename)
			header := fmt.Sprintf("$ sudo %s\n", strings.Join(cap.argv, " "))
			if len(cap.grepFor) > 0 {
				header += "# filtered to lines matching: " + strings.Join(cap.grepFor, ", ") + "\n"
			}
			if werr := os.WriteFile(path, []byte(header+body), 0o644); werr != nil {
				fmt.Printf("    (artifact write failed: %v)\n", werr)
			}
		}
	}
}

// filterLines returns only stdout lines that contain any of needles.
// Empty needles slice → input passes through unchanged. Empty needle
// strings are skipped so a caller can pass `opts.LogicalIP` without
// guarding for the empty case.
func filterLines(body string, needles []string) string {
	var clean []string
	for _, n := range needles {
		if n != "" {
			clean = append(clean, n)
		}
	}
	if len(clean) == 0 {
		return body
	}
	var b strings.Builder
	for _, line := range strings.Split(body, "\n") {
		for _, n := range clean {
			if strings.Contains(line, n) {
				b.WriteString(line)
				b.WriteByte('\n')
				break
			}
		}
	}
	return b.String()
}

func runWithTimeout(cmd *exec.Cmd, timeout time.Duration) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		return fmt.Errorf("timed out after %s", timeout)
	}
}
