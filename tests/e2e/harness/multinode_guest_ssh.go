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

// GuestSSHPort extracts the QEMU hostfwd port for an instance from the
// hosting node's qemu process command line. Reuses ec2helpers.hostfwdRE
// (same `hostfwd=tcp:HOST:PORT-:22` pattern). Polls until the qemu
// process appears (the daemon may still be spawning at the time of the
// first ssh in). Returns 0 if the port is never observable within the
// poll window.
func GuestSSHPort(t *testing.T, hostNode Node, instanceID string, opts ...PollOpt) int {
	t.Helper()
	cfg := applyOpts(pollCfg{timeout: 30 * time.Second, interval: 1 * time.Second}, opts...)
	ssh := NewPeerSSH()
	var port int
	EventuallyErr(t, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), cfg.interval+5*time.Second)
		defer cancel()
		out, err := runPSGrep(ctx, ssh, hostNode, instanceID)
		if err != nil {
			return fmt.Errorf("%s ps grep: %w", hostNode.Name, err)
		}
		m := hostfwdRE.FindStringSubmatch(out)
		if len(m) < 3 {
			return fmt.Errorf("%s no hostfwd port for %s yet", hostNode.Name, instanceID)
		}
		p, err := strconv.Atoi(m[2])
		if err != nil {
			return fmt.Errorf("%s parse hostfwd port %q: %w", hostNode.Name, m[2], err)
		}
		port = p
		return nil
	}, cfg.timeout, cfg.interval)
	return port
}

// GuestSSHEndpoint resolves the (host, port) pair for SSH-ing into a guest
// VM. Prefers the instance's PublicIpAddress when set; falls back to the
// hosting node's WAN IP + QEMU hostfwd port. Mirrors the conditional in
// run-multinode-e2e.sh phase 4.
//
// Returns (host, port, nil) on success; t.Fatal on full failure. Public-IP
// path is the steady-state once OVN's public subnet is wired; the
// hostfwd path is the legacy fallback for VPCs without public routing.
func GuestSSHEndpoint(t *testing.T, c *AWSClient, cluster *Cluster, instanceID string, opts ...PollOpt) (host string, port int) {
	t.Helper()

	inst := WaitForInstanceState(t, c, instanceID, "running", opts...)
	if pub := aws.StringValue(inst.PublicIpAddress); pub != "" {
		return pub, 22
	}

	hostNode := InstanceHostingNode(t, cluster, instanceID)
	if hostNode == nil {
		t.Fatalf("GuestSSHEndpoint: no node hosts %s", instanceID)
	}
	port = GuestSSHPort(t, *hostNode, instanceID, opts...)
	if port == 0 {
		t.Fatalf("GuestSSHEndpoint: hostfwd port not resolved for %s on %s", instanceID, hostNode.Name)
	}
	return hostNode.Addr, port
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

func runSSH(ctx context.Context, args []string) error {
	out, err := exec.CommandContext(ctx, "ssh", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
