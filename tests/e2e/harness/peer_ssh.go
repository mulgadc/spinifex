//go:build e2e

package harness

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// PeerSSH is a minimal SSH wrapper for driving remote cluster nodes in
// LB/multinode scenarios.
type PeerSSH struct {
	User    string
	KeyPath string
	Timeout time.Duration
}

// NewPeerSSH reads SPINIFEX_SSH_KEY (default ~/.ssh/tf-user-ap-southeast-2)
// and SPINIFEX_SSH_USER (default tf-user). Matches the bash peer_ssh helper.
func NewPeerSSH() *PeerSSH {
	return &PeerSSH{
		User:    getenv("SPINIFEX_SSH_USER", "tf-user"),
		KeyPath: getenv("SPINIFEX_SSH_KEY", os.ExpandEnv("$HOME/.ssh/tf-user-ap-southeast-2")),
		Timeout: 10 * time.Second,
	}
}

// Run executes cmd on host over SSH and returns combined stdout+stderr.
// Non-zero exit becomes a non-nil error with stderr in the message.
func (p *PeerSSH) Run(ctx context.Context, host, cmd string) ([]byte, error) {
	args := []string{
		"-i", p.KeyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", fmt.Sprintf("ConnectTimeout=%d", int(p.Timeout.Seconds())),
		"-o", "LogLevel=ERROR",
		fmt.Sprintf("%s@%s", p.User, host),
		cmd,
	}
	out, err := exec.CommandContext(ctx, "ssh", args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("ssh %s@%s: %w: %s", p.User, host, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// Ping returns nil if `hostname` succeeds over SSH. Used as a probe before
// running real remote tests.
func (p *PeerSSH) Ping(ctx context.Context, host string) error {
	_, err := p.Run(ctx, host, "hostname")
	return err
}

// IsReachable wraps Ping with a short timeout. Used by cleanup paths that
// must skip rather than fatal when the host is mid-reboot or already torn
// down. Caps the parent ctx at 10s so cleanup doesn't block teardown.
func (p *PeerSSH) IsReachable(ctx context.Context, host string) bool {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return p.Ping(probeCtx, host) == nil
}
