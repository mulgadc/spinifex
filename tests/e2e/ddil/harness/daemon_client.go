//go:build e2e

package harness

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/mulgadc/spinifex/spinifex/types"
)

// daemonPort is the cluster-manager HTTPS port the daemon binds for /health
// and /local/* (see spinifex/daemon/daemon.go ClusterManager, configured via
// [nodes.<id>.daemon].host). 4432 is the deployed default; 8443 is predastore.
const daemonPort = 4432

// DaemonMode mirrors the daemon's operating mode reported by /local/status.
//
// The typed enum avoids stringly-typed assertions in scenarios. The concrete
// endpoint and JSON schema land with daemon-local-autonomy §1b; this file
// predates that PR and its types will be swapped for the daemon's own when
// it merges.
type DaemonMode string

const (
	ModeUnknown    DaemonMode = ""
	ModeCluster    DaemonMode = "cluster"
	ModeStandalone DaemonMode = "standalone"
)

// LocalStatus is the response shape from GET /local/status.
//
// Placeholder fields only — the authoritative struct ships with
// daemon-local-autonomy §1b. Scenarios that depend on these fields remain
// t.Skip until that epic replaces this definition with an import from the
// daemon package.
type LocalStatus struct {
	Node     string     `json:"node"`
	Mode     DaemonMode `json:"mode"`
	Revision uint64     `json:"revision"`
	NATS     string     `json:"nats"` // "connected" | "disconnected"
}

// LocalInstance is one row of GET /local/instances. Placeholder — see
// LocalStatus.
type LocalInstance struct {
	InstanceID string `json:"instance_id"`
	State      string `json:"state"`
	PID        int    `json:"pid,omitempty"`
}

// DaemonClient is a thin HTTPS client targeting a single daemon's local
// endpoints. It uses one *http.Client per node so connection reuse works
// across calls within a scenario.
type DaemonClient struct {
	http *http.Client
}

// NewDaemonClient returns a DaemonClient with TLS verification disabled
// (the tofu-cluster uses self-signed per-node certs). Timeouts are set to
// 5s so a wedged daemon fails fast enough for scenario retry logic.
func NewDaemonClient() *DaemonClient {
	return &DaemonClient{
		http: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed test certs
			},
		},
	}
}

func (d *DaemonClient) url(node Node, path string) string {
	return "https://" + net.JoinHostPort(node.Addr, strconv.Itoa(daemonPort)) + path
}

func (d *DaemonClient) getJSON(ctx context.Context, node Node, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.url(node, path), nil)
	if err != nil {
		return fmt.Errorf("ddil harness: daemon %s %s: %w", node.Name, path, err)
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return fmt.Errorf("ddil harness: daemon %s %s: %w", node.Name, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ddil harness: daemon %s %s: status %d: %s",
			node.Name, path, resp.StatusCode, string(body))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("ddil harness: daemon %s %s decode: %w", node.Name, path, err)
	}
	return nil
}

// Status returns the daemon's self-reported local status.
//
// Depends on daemon-local-autonomy §1b. Until that lands, the daemon will
// return 404 and callers should t.Skip.
func (d *DaemonClient) Status(ctx context.Context, node Node) (LocalStatus, error) {
	var s LocalStatus
	if err := d.getJSON(ctx, node, "/local/status", &s); err != nil {
		return LocalStatus{}, err
	}
	return s, nil
}

// Instances returns the daemon's locally-known instance list.
//
// Depends on daemon-local-autonomy §1a. Until that lands, the daemon will
// return 404 and callers should t.Skip.
func (d *DaemonClient) Instances(ctx context.Context, node Node) ([]LocalInstance, error) {
	var xs []LocalInstance
	if err := d.getJSON(ctx, node, "/local/instances", &xs); err != nil {
		return nil, err
	}
	return xs, nil
}

// Health hits the daemon's existing /health endpoint. Useful during
// scaffolding validation before the /local/* endpoints exist — confirms
// the harness can reach the daemon at all.
func (d *DaemonClient) Health(ctx context.Context, node Node) (types.NodeHealthResponse, error) {
	var h types.NodeHealthResponse
	if err := d.getJSON(ctx, node, "/health", &h); err != nil {
		return types.NodeHealthResponse{}, err
	}
	return h, nil
}
