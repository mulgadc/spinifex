//go:build e2e

package harness

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

// Multinode service ports. Match bash run-multinode-e2e.sh constants.
const (
	natsMonitorPort      = 8222
	predastoreHealthPort = 8443
	awsgwHealthPort      = 9999
)

// natsRoutezResponse models the NATS /routez subset used to count distinct peers.
type natsRoutezResponse struct {
	Routes []struct {
		RemoteID string `json:"remote_id"`
	} `json:"routes"`
}

// WaitNATSPeers polls every node's NATS /routez until each reports at least want
// distinct peers (timeout 60s, interval 2s). NATS monitor binds 127.0.0.1:8222
// only, so queries run via PeerSSH + curl rather than dialling node.Addr directly.
func (c *Cluster) WaitNATSPeers(t *testing.T, want int, opts ...PollOpt) {
	t.Helper()
	cfg := applyOpts(pollCfg{timeout: 60 * time.Second, interval: 2 * time.Second}, opts...)
	ssh := NewPeerSSH()

	EventuallyErr(t, func() error {
		for _, n := range c.Nodes {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			out, err := ssh.Run(ctx, n.Addr,
				fmt.Sprintf("curl -fsS http://127.0.0.1:%d/routez", natsMonitorPort))
			cancel()
			if err != nil {
				return fmt.Errorf("%s NATS /routez via peer_ssh: %w", n.Name, err)
			}
			var routez natsRoutezResponse
			if err := json.Unmarshal(out, &routez); err != nil {
				return fmt.Errorf("%s NATS /routez decode: %w", n.Name, err)
			}
			seen := map[string]struct{}{}
			for _, r := range routez.Routes {
				if r.RemoteID == "" {
					continue
				}
				seen[r.RemoteID] = struct{}{}
			}
			if len(seen) < want {
				return fmt.Errorf("%s NATS peers=%d want>=%d", n.Name, len(seen), want)
			}
		}
		return nil
	}, cfg.timeout, cfg.interval)
}

// WaitPredastoreHealthy polls every node's Predastore HTTPS endpoint until
// it returns 2xx. Matches bash verify_predastore_cluster (curl -sk per node).
func (c *Cluster) WaitPredastoreHealthy(t *testing.T, opts ...PollOpt) {
	t.Helper()
	c.waitHTTPSPerNode(t, "Predastore", predastoreHealthPort, "/", opts...)
}

// WaitGatewayHealthy polls every node's AWS gateway HTTPS endpoint until
// the TCP+TLS handshake succeeds. Matches bash wait_for_gateway loop.
func (c *Cluster) WaitGatewayHealthy(t *testing.T, opts ...PollOpt) {
	t.Helper()
	c.waitHTTPSPerNode(t, "Gateway", awsgwHealthPort, "/", opts...)
}

// WaitDaemonReady polls each node's gateway by issuing a cheap
// DescribeInstanceTypes against that node's gateway endpoint. Matches bash
// wait_for_daemon_ready — confirms the gateway can route to a live daemon,
// not just that the gateway socket accepts TLS.
func (c *Cluster) WaitDaemonReady(t *testing.T, env *Env, opts ...PollOpt) {
	t.Helper()
	cfg := applyOpts(pollCfg{timeout: 60 * time.Second, interval: 2 * time.Second}, opts...)
	EventuallyErr(t, func() error {
		for _, n := range c.Nodes {
			cli := AWSClientForGateway(t, env, n)
			_, err := cli.EC2.DescribeInstanceTypes(nil)
			if err != nil {
				return fmt.Errorf("%s DescribeInstanceTypes: %w", n.Name, err)
			}
		}
		return nil
	}, cfg.timeout, cfg.interval)
}

func (c *Cluster) waitHTTPSPerNode(t *testing.T, label string, port int, path string, opts ...PollOpt) {
	t.Helper()
	cfg := applyOpts(pollCfg{timeout: 60 * time.Second, interval: 2 * time.Second}, opts...)
	httpc := insecureHTTPClient(cfg.interval)
	EventuallyErr(t, func() error {
		for _, n := range c.Nodes {
			url := fmt.Sprintf("https://%s:%d%s", n.Addr, port, path)
			resp, err := httpc.Get(url)
			if err != nil {
				return fmt.Errorf("%s %s: %w", n.Name, label, err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode >= 500 {
				return fmt.Errorf("%s %s status=%d", n.Name, label, resp.StatusCode)
			}
		}
		return nil
	}, cfg.timeout, cfg.interval)
}

func insecureHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // health-poll across nodes uses self-signed certs
		},
	}
}
