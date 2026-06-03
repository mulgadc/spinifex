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

// DumpVPCFlowDiagnostics emits a triage bundle for VPC/IGW datapath failures
// (e.g. Phase8b SSH-to-public-IP timeout). Dumps the guest's console tail
// (boot + cloud-init state) plus a slim OVN state snapshot to identify
// whether the gateway router has converged. Non-fatal — runs purely for log
// signal so the test's primary Fatal still wins.
//
// Tracked product bugs surfaced by this signal:
//   - mulga-siv-105 (handleIGWAttach lacks flows-ready barrier)
//   - mulga-siv-106 (handleVPCCreate doesn't store spinifex:cidr)
//   - mulga-siv-107 (dnat_and_snat double-delete race)
//   - mulga-siv-111 (EIP-recycle stale OVS state; this is the Phase 1
//     diagnostics surface that closes the bead's first acceptance gate)
//
// Skips silently if OVN tooling / passwordless sudo aren't available, so the
// helper is safe to call from developer laptops too.
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

// dumpDatapathState captures OVS / kernel-side state keyed on the
// external IP being probed. The signal disambiguates between three
// hypotheses on the residual SSH-timeout-despite-barrier failure
// (mulga-siv-111): (1) stale conntrack, (2) stale upstream ARP, (3)
// missing OF flow install. Each capture also writes to a separate
// file under opts.ArtifactDir when set, so the Stage 2 analyzer
// bundler picks them up.
//
// Skipped silently when the IP-specific inputs aren't supplied or the
// shell tools aren't available.
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
