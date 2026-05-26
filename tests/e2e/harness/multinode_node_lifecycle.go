//go:build e2e

package harness

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// StopNode stops every spinifex unit on a remote node via
// `sudo systemctl stop spinifex.target`. Used by phase 8 (node failure).
// Bash issues the same command and tolerates a non-zero exit because the
// cluster shutdown sequence racily kills the SSH connection — we mirror
// that lenience by logging instead of fatal-ing.
func StopNode(t *testing.T, node Node) {
	t.Helper()
	ssh := NewPeerSSH()
	// spinifex.target has 6+ units (predastore, NATS, awsgw, vpcd, daemon, ui).
	// stop takes 30-60s in practice; budget 3min to cover slow shutdowns.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := ssh.Run(ctx, node.Addr, "sudo systemctl stop spinifex.target"); err != nil {
		t.Logf("StopNode %s: %v (proceeding — bash treats this as non-fatal)", node.Name, err)
	}
}

// StartNode brings the spinifex.target back up on a remote node. Used by
// phase 9 (recovery) and as a t.Cleanup safety net in phase 8 so a
// cancelled run doesn't leave the cluster degraded for the next suite.
func StartNode(t *testing.T, node Node) {
	t.Helper()
	ssh := NewPeerSSH()
	// systemctl start on spinifex.target blocks until every dependent unit
	// is Active=active — predastore + NATS + awsgw + vpcd + daemon + ui can
	// take 60-90s combined. 30s was too tight and produced "signal: killed"
	// SSH terminations under context cancel; bump to 5min.
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
