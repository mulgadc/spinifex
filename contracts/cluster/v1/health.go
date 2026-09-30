package clusterv1

import "fmt"

// NodeHealthSubject returns the NATS route for one daemon's health response.
// It intentionally performs no node-ID validation: callers have always
// supplied the configured node identifier directly to NATS, and this
// structural move must preserve that behaviour.
func NodeHealthSubject(node string) string {
	return fmt.Sprintf("spinifex.admin.%s.health", node)
}

// NodeHealthResponse is the daemon health representation returned by both the
// per-node NATS health route and the daemon HTTPS /health endpoint.
type NodeHealthResponse struct {
	Node          string            `json:"node"`
	Status        string            `json:"status"`
	ConfigHash    string            `json:"config_hash"`
	Epoch         uint64            `json:"epoch"`
	Uptime        int64             `json:"uptime"`
	Services      []string          `json:"services"`
	ServiceHealth map[string]string `json:"service_health,omitempty"`
}
