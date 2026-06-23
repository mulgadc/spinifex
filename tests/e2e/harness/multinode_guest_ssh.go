//go:build e2e

package harness

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
)

// GuestSSHEndpoint resolves (host, 22) for SSH-ing into a guest VM. No
// qemu-hostfwd fallback: hostfwd bypasses the OVN datapath (see
// InstancePublicSSHHost). A running instance with no public IP is a hard failure.
func GuestSSHEndpoint(t *testing.T, c *AWSClient, cluster *Cluster, instanceID string, opts ...PollOpt) (host string, port int) {
	t.Helper()
	_ = cluster // retained for signature stability; node hostfwd lookup removed
	inst := WaitForInstanceState(t, c, instanceID, "running", opts...)
	pub := aws.StringValue(inst.PublicIpAddress)
	if pub == "" {
		t.Fatalf("GuestSSHEndpoint: instance %s has no public IP; "+
			"qemu-hostfwd fallback is disabled (it bypasses the OVN datapath)", instanceID)
	}
	return pub, 22
}

// GuestSSHReady polls SSH into the named instance until a probe command
// succeeds. Pemfile is the test-scoped private key written by EnsureKeyPair;
// user defaults to ec2-user (the cloud-init account that spinifex injects
// regardless of distro family — see handlers/ec2/instance/service_impl.go).
func GuestSSHReady(t *testing.T, host string, port int, user, pemfile string, opts ...PollOpt) {
	t.Helper()
	cfg := applyOpts(pollCfg{timeout: 60 * time.Second, interval: 1 * time.Second}, opts...)
	target := fmt.Sprintf("%s@%s", user, host)

	EventuallyErr(t, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), cfg.interval+2*time.Second)
		defer cancel()
		args := []string{
			"-i", pemfile,
			"-p", strconv.Itoa(port),
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-o", "ConnectTimeout=2",
			"-o", "BatchMode=yes",
			"-o", "LogLevel=ERROR",
			target,
			"echo ready",
		}
		if err := runSSH(ctx, args); err != nil {
			return fmt.Errorf("ssh %s:%d: %w", host, port, err)
		}
		return nil
	}, cfg.timeout, cfg.interval)
}

// TryGuestSSHReady is the non-fatal twin of GuestSSHReady: it polls SSH up to
// timeout and returns whether the probe ever succeeded instead of t.Fatal-ing.
// Callers use a false return to dump datapath diagnostics before failing — a
// fresh cross-node public-IP guest unreachable for the full window is the
// SNAT/DNAT flow-install flake signature, only diagnosable from CI artifacts.
func TryGuestSSHReady(host string, port int, user, pemfile string, timeout time.Duration) bool {
	target := fmt.Sprintf("%s@%s", user, host)
	args := []string{
		"-i", pemfile,
		"-p", strconv.Itoa(port),
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=2",
		"-o", "BatchMode=yes",
		"-o", "LogLevel=ERROR",
		target,
		"echo ready",
	}
	deadline := time.Now().Add(timeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		err := runSSH(ctx, args)
		cancel()
		if err == nil {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Second)
	}
}

func runSSH(ctx context.Context, args []string) error {
	out, err := exec.CommandContext(ctx, "ssh", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
