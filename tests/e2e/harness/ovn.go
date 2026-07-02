//go:build e2e

package harness

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// SkipIfNoOVN calls t.Skip when ovn-nbctl isn't on PATH or passwordless sudo
// isn't available. Developer laptops don't have OVN; CI VMs have both.
func SkipIfNoOVN(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ovn-nbctl"); err != nil {
		t.Skipf("ovn-nbctl not on PATH (%v); skipping OVN check", err)
	}
	// `sudo -n true` returns 0 only if a sudoers entry allows passwordless
	// for the current user. Anything else (prompt, deny, missing sudo) skips.
	if err := exec.Command("sudo", "-n", "true").Run(); err != nil {
		t.Skipf("passwordless sudo unavailable (%v); skipping OVN check", err)
	}
}

// OvnNbctl runs `sudo -n ovn-nbctl <args...>`. Returns trimmed stdout on
// success; t.Fatal on non-zero exit. Wrap in Eventually for propagation polls.
func OvnNbctl(t *testing.T, args ...string) string {
	t.Helper()
	return runOvn(t, "ovn-nbctl", args...)
}

// OvnSbctl is the SB-database equivalent of OvnNbctl.
func OvnSbctl(t *testing.T, args ...string) string {
	t.Helper()
	return runOvn(t, "ovn-sbctl", args...)
}

// OvnTrace runs `sudo -n ovn-trace <args...>` and returns the detailed pipeline
// trace. Pass `--ct=...` options first, then DATAPATH, then MICROFLOW.
func OvnTrace(t *testing.T, args ...string) string {
	t.Helper()
	return runOvn(t, "ovn-trace", args...)
}

// OvsVsctl runs `sudo -n ovs-vsctl <args...>` against the local chassis OVS DB.
// Used to inspect per-chassis datapath state (e.g. br-imds ports) that the
// cluster-shared NB/SB do not carry.
func OvsVsctl(t *testing.T, args ...string) string {
	t.Helper()
	return runOvn(t, "ovs-vsctl", args...)
}

// OvsOfctl runs `sudo -n ovs-ofctl <args...>` against the local chassis OVS.
func OvsOfctl(t *testing.T, args ...string) string {
	t.Helper()
	return runOvn(t, "ovs-ofctl", args...)
}

func runOvn(t *testing.T, tool string, args ...string) string {
	t.Helper()
	full := append([]string{"-n", tool}, args...)
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("sudo", full...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("sudo %s %s failed: %v\nstderr: %s", tool, strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

// WaitForPortGroupMember polls until OVN port_group pg contains lsp (10s/1s).
// Uses ovn-nbctl to look up the LSP UUID and confirm it appears in the port_group.
func WaitForPortGroupMember(t *testing.T, pg, lsp string, opts ...PollOpt) {
	t.Helper()
	cfg := applyOpts(pollCfg{timeout: 10 * time.Second, interval: 1 * time.Second}, opts...)

	EventuallyErr(t, func() error {
		lspUUID := OvnNbctl(t, "--no-leader-only", "--bare", "--columns=_uuid",
			"find", "logical_switch_port", "name="+lsp)
		if lspUUID == "" {
			return fmt.Errorf("LSP %s not found in NB", lsp)
		}
		ports := OvnNbctl(t, "--no-leader-only", "--bare", "--columns=ports",
			"find", "port_group", "name="+pg)
		if !strings.Contains(ports, lspUUID) {
			return fmt.Errorf("LSP %s (%s) not in port_group %s (ports=%s)", lsp, lspUUID, pg, ports)
		}
		return nil
	}, cfg.timeout, cfg.interval)
}
