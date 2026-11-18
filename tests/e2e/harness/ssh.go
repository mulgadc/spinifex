//go:build e2e

package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"

	"golang.org/x/crypto/ssh"
)

// SSH is the transport scenarios use to run commands on a remote Node.
//
// It is an interface so dry-run mode and unit tests can stub it without
// opening real network connections. Run returns the command's combined
// stdout+stderr and a non-nil error if the remote command exited non-zero or
// the session failed.
type SSH interface {
	Run(ctx context.Context, node Node, cmd string) ([]byte, error)
	Close() error
}

// RunGuestSSH executes cmd in a guest addressed by an SSHTarget. It returns
// combined output so callers can retry assertions without terminating a test.
func RunGuestSSH(ctx context.Context, target SSHTarget, cmd string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "ssh", guestSSHArgs(target, 5, cmd)...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("guest ssh %s@%s:%d: %w: %s", target.User, target.Host, target.Port, err, out)
	}
	return out, nil
}

// guestSSHArgs is the non-interactive, no-host-key-check ssh(1) argument list
// every guest probe uses; connectTimeout is in seconds.
func guestSSHArgs(tgt SSHTarget, connectTimeout int, cmd string) []string {
	return guestSSHArgsAt(tgt, connectTimeout, "ERROR", cmd)
}

// guestSSHArgsAt is guestSSHArgs at an explicit ssh LogLevel. Above ERROR,
// ssh also prints a host-key notice, so only use it where stderr is separate.
func guestSSHArgsAt(tgt SSHTarget, connectTimeout int, logLevel, cmd string) []string {
	return []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=" + logLevel,
		"-o", "ConnectTimeout=" + strconv.Itoa(connectTimeout),
		"-o", "BatchMode=yes",
		"-p", strconv.Itoa(tgt.Port),
		"-i", tgt.KeyPath,
		tgt.User + "@" + tgt.Host,
		cmd,
	}
}

// RunSSH runs command in the guest and returns stdout. It t.Fatals on a
// non-zero exit so callers can chain assertions on the output.
func RunSSH(t *testing.T, tgt SSHTarget, command string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("ssh", guestSSHArgs(tgt, 5, command)...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("ssh %s@%s:%d %q failed: %v\nstderr: %s",
			tgt.User, tgt.Host, tgt.Port, command, err, stderr.String())
	}
	return stdout.String()
}

// SSHSettleBudget bounds RunSSHSettled: long enough for one vpcd reconcile
// pass or a guest finishing boot, short against SSHReadyBudget.
const SSHSettleBudget = 30 * time.Second

// RunSSHSettled is RunSSH that retries ssh's exit 255 within budget, logging
// ssh's INFO-level stderr for each drop. A remote command's own 255 looks the
// same, so pass only read-only commands that are safe to run twice.
func RunSSHSettled(t *testing.T, tgt SSHTarget, command string, budget time.Duration) string {
	t.Helper()
	args := guestSSHArgsAt(tgt, 5, "INFO", command)
	stdout, stderr, err := retrySSHTransport(budget, 2*time.Second,
		func(ctx context.Context) (string, string, error) {
			var o, e bytes.Buffer
			cmd := exec.CommandContext(ctx, "ssh", args...)
			cmd.Stdout = &o
			cmd.Stderr = &e
			err := cmd.Run()
			return o.String(), e.String(), err
		},
		func(attempt int, stderr string) {
			Step(t, "ssh %s@%s:%d %q exited 255 on attempt %d; retrying\nstderr: %s",
				tgt.User, tgt.Host, tgt.Port, command, attempt, strings.TrimSpace(stderr))
		})
	if err != nil {
		t.Fatalf("ssh %s@%s:%d %q failed within %s: %v\nstderr: %s",
			tgt.User, tgt.Host, tgt.Port, command, budget, err, stderr)
	}
	return stdout
}

// retrySSHTransport runs attempt under one deadline of budget, pausing and
// retrying while it exits 255. Every attempt shares the deadline, so a stalled
// session is killed at the budget rather than holding the caller past it.
func retrySSHTransport(budget, pause time.Duration,
	attempt func(context.Context) (stdout, stderr string, err error),
	onRetry func(attempt int, stderr string),
) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	for n := 1; ; n++ {
		stdout, stderr, err := attempt(ctx)
		if err == nil || !isSSHExit255(err) || ctx.Err() != nil {
			return stdout, stderr, err
		}
		onRetry(n, stderr)
		select {
		case <-ctx.Done():
			return stdout, stderr, err
		case <-time.After(pause):
		}
	}
}

func isSSHExit255(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 255
}

// RunSSHCombined runs command in the guest and returns combined stdout+stderr
// regardless of exit status, for probes (ping, curl) where a non-zero exit is
// an expected outcome.
func RunSSHCombined(tgt SSHTarget, command string) (string, error) {
	out, err := exec.Command("ssh", guestSSHArgs(tgt, 5, command)...).CombinedOutput()
	return string(out), err
}

// SSHReadyBudget bounds the first SSH-handshake pass. Baremetal boots q35
// guests slower (OVMF + NBD disks), so the harness widens it via
// SPINIFEX_SSH_READY_TIMEOUT (Go duration); default 3m bounds shared runners.
var SSHReadyBudget = durationEnv("SPINIFEX_SSH_READY_TIMEOUT", 3*time.Minute)

// sshReprimeBudget bounds the second SSH pass after an ARP re-prime; short
// because a helpful re-prime lands quickly. SPINIFEX_SSH_REPRIME_TIMEOUT.
var sshReprimeBudget = durationEnv("SPINIFEX_SSH_REPRIME_TIMEOUT", 60*time.Second)

// wanBridge is the host bridge an EIP is presented on, i.e. the L2 segment the
// runner ARPs to reach a guest's public IP. Override via SPINIFEX_WAN_BRIDGE.
var wanBridge = stringEnv("SPINIFEX_WAN_BRIDGE", "br-wan")

func durationEnv(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

func stringEnv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// SSHHealth is a suite's sticky SSH-datapath verdict: the first handshake
// timeout marks it broken and later SSH-dependent tests skip rather than
// re-run the same multi-minute wait. Each suite keeps its own value.
type SSHHealth struct {
	broken atomic.Bool
}

// Require skips t if an earlier SSH probe in this suite has failed.
func (h *SSHHealth) Require(t *testing.T) {
	t.Helper()
	if h.broken.Load() {
		t.Skipf("skipping: earlier SSH probe failed; " +
			"not retrying to keep suite time bounded")
	}
}

// WaitReady waits for a full SSH handshake to host:port. TCP reachability is
// not enough: sshd accepts while pam/cloud-init finish, and the first real
// command then times out during banner exchange.
func (h *SSHHealth) WaitReady(t *testing.T, host string, port int, keyPath string) {
	t.Helper()
	h.WaitReadyOr(t, host, port, keyPath, nil)
}

// WaitReadyOr is WaitReady that calls onMiss, when not nil, as soon as the
// first budget misses and before the re-prime, so a diagnostic dump captures
// the guest's state while it is still failing.
func (h *SSHHealth) WaitReadyOr(t *testing.T, host string, port int, keyPath string, onMiss func()) {
	t.Helper()
	h.Require(t)
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	Step(t, "waiting for SSH handshake %s", addr)
	ready := waitReadyDecision(
		func(budget time.Duration) bool { return h.handshakeWithin(host, port, keyPath, budget) },
		func() {
			Step(t, "SSH handshake %s missed %s budget; re-priming ARP + retrying", addr, SSHReadyBudget)
			reprimeSSHReachability(t, host)
		},
		onMiss)
	if ready {
		return
	}
	h.broken.Store(true)
	t.Fatalf("Eventually: condition not met within %s (+%s after ARP re-prime): "+
		"[SSH handshake %s never completed] "+
		"(sticky-skip enabled for downstream)", SSHReadyBudget, sshReprimeBudget, addr)
}

// TryReady is WaitReady without the re-prime or t.Fatal: it reports whether
// the handshake completed within budget and never marks the suite broken. It
// skips t once the suite is broken, as Require does, so no caller reports a
// timeout for a wait that never happened.
func (h *SSHHealth) TryReady(t *testing.T, host string, port int, keyPath string, budget time.Duration) bool {
	t.Helper()
	h.Require(t)
	return h.handshakeWithin(host, port, keyPath, budget)
}

// waitReadyDecision tries the first budget, then on a miss calls onMiss and
// reprime and retries once. OVN emits no GARP on a same-chassis EIP rebind, so
// a decayed host neigh entry can blackhole host->guest SSH past the budget.
func waitReadyDecision(attempt func(time.Duration) bool, reprime, onMiss func()) bool {
	if attempt(SSHReadyBudget) {
		return true
	}
	if onMiss != nil {
		onMiss()
	}
	reprime()
	return attempt(sshReprimeBudget)
}

// handshakeWithin polls for a full SSH handshake until budget runs out.
func (h *SSHHealth) handshakeWithin(host string, port int, keyPath string, budget time.Duration) bool {
	tgt := SSHTarget{User: "ubuntu", Host: host, Port: port, KeyPath: keyPath}
	return pollHandshake(budget, sshAttemptFloor, 2*time.Second, func(ctx context.Context) bool {
		return exec.CommandContext(ctx, "ssh", guestSSHArgs(tgt, 3, "true")...).Run() == nil
	})
}

// sshAttemptFloor is the least time one handshake attempt gets, so the last
// attempt before the deadline is not killed before it can connect.
const sshAttemptFloor = 5 * time.Second

// pollHandshake retries attempt until it succeeds or budget runs out. Each
// attempt is cancelled at the deadline (or floor after it starts, if later),
// so a stalled ssh cannot hold the wait past its budget.
func pollHandshake(budget, floor, pause time.Duration, attempt func(context.Context) bool) bool {
	deadline := time.Now().Add(budget)
	for {
		stop := deadline
		if f := time.Now().Add(floor); f.After(stop) {
			stop = f
		}
		ctx, cancel := context.WithDeadline(context.Background(), stop)
		ok := attempt(ctx)
		cancel()
		if ok {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(pause)
	}
}

// reprimeSSHReachability best-effort flushes the host ARP entry for host on
// the WAN bridge so the next pass re-resolves it. Needs ip(8) and passwordless
// sudo; non-fatal so a missing tool never masks the real result.
func reprimeSSHReachability(t *testing.T, host string) {
	t.Helper()
	if _, err := exec.LookPath("ip"); err != nil {
		Step(t, "skip ARP re-prime: ip(8) unavailable (%v)", err)
		return
	}
	if err := exec.Command("sudo", "-n", "true").Run(); err != nil {
		Step(t, "skip ARP re-prime: passwordless sudo unavailable (%v)", err)
		return
	}
	Step(t, "flushing host neigh for %s dev %s", host, wanBridge)
	out, err := exec.Command("sudo", "-n", "ip", "neigh", "flush", "to", host, "dev", wanBridge).CombinedOutput()
	if err != nil {
		Step(t, "ARP re-prime flush failed (best-effort): %v: %s", err, strings.TrimSpace(string(out)))
	}
}

// WaitForInstanceStateSoft is the cleanup-time analogue of
// WaitForInstanceState: it polls and returns an error instead of t.Fatal.
func WaitForInstanceStateSoft(c *AWSClient, id, target string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
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
			return fmt.Errorf("instance %s did not reach %s within %s", id, target, wait)
		}
		time.Sleep(2 * time.Second)
	}
}

// sshClient is the production SSH transport, backed by
// golang.org/x/crypto/ssh. One client is opened per (user, key, node) tuple
// on first use and cached for the harness lifetime.
type sshClient struct {
	user       string
	signer     ssh.Signer
	conns      map[string]*ssh.Client
	connectDur time.Duration
	runDur     time.Duration
}

var _ SSH = (*sshClient)(nil)

// NewSSH returns an SSH transport configured from a Cluster. Host keys are not
// verified: the harness targets short-lived infra where TOFU adds noise and no
// security benefit.
func NewSSH(cluster *Cluster) (SSH, error) {
	keyBytes, err := os.ReadFile(cluster.SSHKeyPath)
	if err != nil {
		return nil, fmt.Errorf("e2e harness: read ssh key %s: %w", cluster.SSHKeyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("e2e harness: parse ssh key %s: %w", cluster.SSHKeyPath, err)
	}
	return &sshClient{
		user:       cluster.SSHUser,
		signer:     signer,
		conns:      make(map[string]*ssh.Client),
		connectDur: 10 * time.Second,
		runDur:     2 * time.Minute,
	}, nil
}

func (s *sshClient) dial(node Node) (*ssh.Client, error) {
	if c, ok := s.conns[node.Addr]; ok {
		return c, nil
	}
	cfg := &ssh.ClientConfig{
		User:            s.user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(s.signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // test harness, not production
		Timeout:         s.connectDur,
	}
	c, err := ssh.Dial("tcp", net.JoinHostPort(node.Addr, "22"), cfg)
	if err != nil {
		return nil, fmt.Errorf("e2e harness: ssh dial %s: %w", node.Addr, err)
	}
	s.conns[node.Addr] = c
	return c, nil
}

func (s *sshClient) Run(ctx context.Context, node Node, cmd string) ([]byte, error) {
	client, err := s.dial(node)
	if err != nil {
		return nil, err
	}
	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("e2e harness: ssh new session %s: %w", node.Name, err)
	}
	defer func() { _ = session.Close() }()

	// Signal(KILL) is the portable way to interrupt a running command.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = session.Signal(ssh.SIGKILL)
			_ = session.Close()
		case <-done:
		}
	}()

	// CombinedOutput captures stdout+stderr through x/crypto/ssh's mutex-guarded
	// singleWriter. Assigning one bytes.Buffer to both session.Stdout and
	// session.Stderr races: the library copies the two streams in separate
	// goroutines and bytes.Buffer is not concurrent-safe.
	out, err := session.CombinedOutput(cmd)
	if err != nil {
		return out, fmt.Errorf("e2e harness: ssh run on %s (%q): %w\noutput: %s",
			node.Name, cmd, err, out)
	}
	return out, nil
}

func (s *sshClient) Close() error {
	var firstErr error
	for addr, c := range s.conns {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("e2e harness: ssh close %s: %w", addr, err)
		}
		delete(s.conns, addr)
	}
	return firstErr
}
