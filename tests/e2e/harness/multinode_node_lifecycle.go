//go:build e2e

package harness

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// StopNode stops every spinifex unit on a remote node via systemctl. Non-fatal:
// the cluster shutdown sequence can racily kill the SSH connection.
func StopNode(t *testing.T, node Node) {
	t.Helper()
	ssh := NewPeerSSH()
	// 3min covers slow 6+ unit shutdowns (predastore, NATS, awsgw, vpcd, daemon, ui).
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := ssh.Run(ctx, node.Addr, "sudo systemctl stop spinifex.target"); err != nil {
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
