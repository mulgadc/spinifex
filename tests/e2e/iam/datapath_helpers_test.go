//go:build e2e

package iam

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// SSH / datapath helpers ported from the single suite so the IMDS and
// instance-profile tests run self-contained inside iam. The single suite keeps
// its own copy — other single tests still use these. They are separate test
// binaries (separate processes), so the sticky-skip state below is per-suite.

// sshDatapathBroken is set the first time an SSH probe in this suite times out.
// Downstream SSH-dependent assertions call requireSSHHealthy(t) and skip rather
// than re-running the same multi-minute timeout against the same broken datapath.
var sshDatapathBroken atomic.Bool

// sshReadyBudget bounds the first SSH-handshake pass. Widen via
// SPINIFEX_SSH_READY_TIMEOUT (Go duration); default 3m bounds shared runners.
var sshReadyBudget = resolveDurationEnv("SPINIFEX_SSH_READY_TIMEOUT", 3*time.Minute)

// sshReprimeBudget bounds the second SSH pass after an ARP re-prime. Short by
// design — if the re-prime helps, the handshake lands quickly. Override via
// SPINIFEX_SSH_REPRIME_TIMEOUT.
var sshReprimeBudget = resolveDurationEnv("SPINIFEX_SSH_REPRIME_TIMEOUT", 60*time.Second)

// wanBridge is the host bridge an EIP is presented on — the L2 segment the
// runner ARPs to reach a guest's public IP. Default br-wan; override for
// non-standard pool fixtures via SPINIFEX_WAN_BRIDGE.
var wanBridge = resolveStringEnv("SPINIFEX_WAN_BRIDGE", "br-wan")

func resolveDurationEnv(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

func resolveStringEnv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// requireSSHHealthy skips the calling test if a prior SSH probe in this suite
// has already failed, cutting the cascade cost of a broken datapath.
func requireSSHHealthy(t *testing.T) {
	t.Helper()
	if sshDatapathBroken.Load() {
		t.Skipf("skipping: earlier SSH probe failed; " +
			"not retrying to keep suite time bounded")
	}
}

// waitForSSHReady probes a full SSH handshake (BatchMode + ConnectTimeout)
// against host:port, retrying until the daemon completes banner exchange.
// TCP-port reachability alone is insufficient — sshd accepts the connect
// while pam/cloud-init are still finishing, and the first real runSSH then
// hits "Connection timed out during banner exchange".
func waitForSSHReady(t *testing.T, host string, port int, keyPath string) {
	t.Helper()
	requireSSHHealthy(t)
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	harness.Step(t, "waiting for SSH handshake %s", addr)
	if trySSHReady(host, port, keyPath, sshReadyBudget) {
		return
	}
	// First budget elapsed. OVN emits no GARP on a same-chassis EIP rebind, so a
	// decayed host neigh entry for the EIP can blackhole host->guest SSH until it
	// ages out — past our budget. Re-prime the ARP entry and retry once before
	// declaring the datapath broken, riding out the transient learning gap.
	harness.Step(t, "SSH handshake %s missed %s budget; re-priming ARP + retrying", addr, sshReadyBudget)
	reprimeSSHReachability(t, host)
	if trySSHReady(host, port, keyPath, sshReprimeBudget) {
		return
	}
	sshDatapathBroken.Store(true)
	t.Fatalf("Eventually: condition not met within %s (+%s after ARP re-prime): "+
		"[SSH handshake %s never completed] "+
		"(sticky-skip enabled for downstream)", sshReadyBudget, sshReprimeBudget, addr)
}

// reprimeSSHReachability best-effort flushes the host ARP entry for the EIP on
// the WAN bridge, forcing a fresh broadcast re-resolution before the next SSH
// pass. Flush (not replace) is mode-agnostic: it needs no MAC and can't install
// a wrong centralised-mode entry. Gated on ip + passwordless sudo like the IMDS
// datapath dump; non-fatal so a missing tool never masks the real result.
func reprimeSSHReachability(t *testing.T, host string) {
	t.Helper()
	if _, err := exec.LookPath("ip"); err != nil {
		harness.Step(t, "skip ARP re-prime: ip(8) unavailable (%v)", err)
		return
	}
	if err := exec.Command("sudo", "-n", "true").Run(); err != nil {
		harness.Step(t, "skip ARP re-prime: passwordless sudo unavailable (%v)", err)
		return
	}
	harness.Step(t, "flushing host neigh for %s dev %s", host, wanBridge)
	out, err := exec.Command("sudo", "-n", "ip", "neigh", "flush", "to", host, "dev", wanBridge).CombinedOutput()
	if err != nil {
		harness.Step(t, "ARP re-prime flush failed (best-effort): %v: %s", err, strings.TrimSpace(string(out)))
	}
}

// waitForSSHHandshake is an alias kept for the imds_test.go call site.
func waitForSSHHandshake(t *testing.T, host string, port int, keyPath string) {
	waitForSSHReady(t, host, port, keyPath)
}

// trySSHReady mirrors waitForSSHReady but returns reachability as a bool
// instead of t.Fatal-ing on timeout.
func trySSHReady(host string, port int, keyPath string, timeout time.Duration) bool {
	if sshDatapathBroken.Load() {
		return false
	}
	deadline := time.Now().Add(timeout)
	for {
		cmd := exec.Command("ssh",
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-o", "LogLevel=ERROR",
			"-o", "ConnectTimeout=3",
			"-o", "BatchMode=yes",
			"-p", strconv.Itoa(port),
			"-i", keyPath,
			"ec2-user@"+host,
			"true")
		if cmd.Run() == nil {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Second)
	}
}

// runSSH is a thin wrapper around `ssh` matching the option set used by
// harness.LsblkRootGiB. Returns stdout. t.Fatal on non-zero exit so callers
// can chain assertions on the output without nil-checking err.
func runSSH(t *testing.T, tgt harness.SSHTarget, command string) string {
	t.Helper()
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=5",
		"-o", "BatchMode=yes",
		"-p", strconv.Itoa(tgt.Port),
		"-i", tgt.KeyPath,
		tgt.User + "@" + tgt.Host,
		command,
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("ssh", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("ssh %s@%s:%d %q failed: %v\nstderr: %s",
			tgt.User, tgt.Host, tgt.Port, command, err, stderr.String())
	}
	return stdout.String()
}

// runSSHCombined runs `command` over SSH against tgt and returns combined
// stdout+stderr regardless of exit status. Unlike runSSH it does not t.Fatal
// on non-zero exit — needed for probes (e.g. curl status codes) where a
// non-zero exit is an expected outcome.
func runSSHCombined(tgt harness.SSHTarget, command string) (string, error) {
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=5",
		"-o", "BatchMode=yes",
		"-p", strconv.Itoa(tgt.Port),
		"-i", tgt.KeyPath,
		tgt.User + "@" + tgt.Host,
		command,
	}
	var combined bytes.Buffer
	cmd := exec.Command("ssh", args...)
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	err := cmd.Run()
	return combined.String(), err
}

// primaryENI returns the NetworkInterfaceId of an instance's first ENI.
// t.Fatal if the instance has no ENI — every running EC2 instance must.
func primaryENI(t *testing.T, inst *ec2.Instance) string {
	t.Helper()
	if len(inst.NetworkInterfaces) == 0 {
		t.Fatalf("instance %s has no NetworkInterfaces", aws.StringValue(inst.InstanceId))
	}
	eni := aws.StringValue(inst.NetworkInterfaces[0].NetworkInterfaceId)
	if eni == "" {
		t.Fatalf("instance %s primary ENI has empty NetworkInterfaceId", aws.StringValue(inst.InstanceId))
	}
	return eni
}

// waitForInstanceStateSoft is the cleanup-time analogue of
// harness.WaitForInstanceState — no t.Fatal, just polls and returns.
func waitForInstanceStateSoft(c *harness.AWSClient, id, target string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		out, err := c.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
			InstanceIds: []*string{aws.String(id)},
		})
		if err == nil && len(out.Reservations) > 0 && len(out.Reservations[0].Instances) > 0 {
			if aws.StringValue(out.Reservations[0].Instances[0].State.Name) == target {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("instance %s did not reach %s within %s", id, target, timeout)
		}
		time.Sleep(2 * time.Second)
	}
}

// detectPoolMode reads external_mode from spinifex.toml. Defaults to false
// (dev_networking) which is the single-node CI fixture. Any non-empty
// external_mode value ("pool" / "nat") means external IPAM is in play, which
// gates the IMDS probe-VPC public-IP path.
func detectPoolMode(env *harness.Env) bool {
	cfg := os.ExpandEnv("$HOME/spinifex/config/spinifex.toml")
	if env.ConfigDir != "" {
		cfg = filepath.Join(env.ConfigDir, "spinifex.toml")
	}
	f, err := os.Open(cfg)
	if err != nil {
		return false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	inNetwork := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") {
			inNetwork = line == "[network]"
			continue
		}
		if !inNetwork {
			continue
		}
		if !strings.HasPrefix(line, "external_mode") {
			continue
		}
		// external_mode = "pool" — quoted value, anything non-empty == pool mode.
		if i := bytes.IndexByte([]byte(line), '='); i >= 0 {
			val := strings.TrimSpace(line[i+1:])
			val = strings.Trim(val, "\"'")
			return val != ""
		}
	}
	return false
}
