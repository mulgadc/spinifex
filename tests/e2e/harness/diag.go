//go:build e2e

package harness

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
)

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
//
// Skips silently if OVN tooling / passwordless sudo aren't available, so the
// helper is safe to call from developer laptops too.
func DumpVPCFlowDiagnostics(t *testing.T, c *AWSClient, instanceID, label string) {
	t.Helper()
	fmt.Printf("\n%s%s── DIAGNOSTICS: %s (instance=%s) ──%s\n",
		colorBold, colorCyan, label, instanceID, colorReset)

	dumpConsoleOutput(t, c, instanceID)
	dumpOVNState(t, instanceID)

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
