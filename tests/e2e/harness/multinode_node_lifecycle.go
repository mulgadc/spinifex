//go:build e2e

package harness

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// nodeServiceUnits are the PartOf=spinifex.target units StopNode stops to
// simulate a node outage. spinifex-shutdown.service is deliberately excluded:
// its ExecStop drains guests to stopped, which a hard outage must not do.
var nodeServiceUnits = []string{
	"spinifex-ui.service",
	"spinifex-vpcd.service",
	"spinifex-awsgw.service",
	"spinifex-daemon.service",
	"spinifex-viperblock.service",
	"spinifex-predastore.service",
	"spinifex-nats.service",
}

// StopNode simulates a hard node outage by stopping the spinifex service units
// directly (not spinifex.target), so guests keep running (daemon
// KillMode=process) and the target's drain ExecStop never fires. Non-fatal:
// the shutdown sequence can racily kill the SSH connection.
func StopNode(t *testing.T, node Node) {
	t.Helper()
	ssh := NewPeerSSH()
	// 3min covers slow shutdowns of all seven units (predastore, NATS, awsgw, vpcd, daemon, ui).
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := "sudo systemctl stop " + strings.Join(nodeServiceUnits, " ")
	if _, err := ssh.Run(ctx, node.Addr, cmd); err != nil {
		t.Logf("StopNode %s: %v (proceeding — bash treats this as non-fatal)", node.Name, err)
	}
}

// StartNode brings spinifex.target back up on a remote node. Also used as a
// t.Cleanup safety net so a cancelled run doesn't leave the cluster degraded.
func StartNode(t *testing.T, node Node) {
	t.Helper()
	ssh := NewPeerSSH()
	// 5min: systemctl blocks until all units are Active; 6 units can take 60-90s.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := ssh.Run(ctx, node.Addr, "sudo systemctl start spinifex.target"); err != nil {
		t.Fatalf("StartNode %s: %v", node.Name, err)
	}
}

// WaitNodeServiceReady polls a node's HTTPS gateway until TLS handshake
// succeeds. Useful after StartNode while the service stack restarts.
// Distinct from WaitGatewayHealthy (cluster-wide) so a single recovering
// node can be tracked without re-checking all peers.
func WaitNodeServiceReady(t *testing.T, node Node, opts ...PollOpt) {
	t.Helper()
	cfg := applyOpts(pollCfg{timeout: 60 * time.Second, interval: 2 * time.Second}, opts...)
	httpc := insecureHTTPClient(cfg.interval)
	url := fmt.Sprintf("https://%s:%d/", node.Addr, awsgwHealthPort)
	EventuallyErr(t, func() error {
		resp, err := httpc.Get(url)
		if err != nil {
			return fmt.Errorf("%s gateway %s: %w", node.Name, url, err)
		}
		_ = resp.Body.Close()
		return nil
	}, cfg.timeout, cfg.interval)
}
