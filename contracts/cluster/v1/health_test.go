package clusterv1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNodeHealthResponseJSONContract(t *testing.T) {
	data, err := json.Marshal(NodeHealthResponse{
		Node:       "node-a",
		Status:     "running",
		ConfigHash: "abc123",
		Epoch:      7,
		Uptime:     42,
		Services:   []string{"nats", "spinifex"},
		ServiceHealth: map[string]string{
			"nats": "ok",
		},
	})
	require.NoError(t, err)
	require.JSONEq(t, `{
		"node":"node-a",
		"status":"running",
		"config_hash":"abc123",
		"epoch":7,
		"uptime":42,
		"services":["nats","spinifex"],
		"service_health":{"nats":"ok"}
	}`, string(data))
}

func TestNodeHealthSubject(t *testing.T) {
	require.Equal(t, "spinifex.admin.node-a.health", NodeHealthSubject("node-a"))
}
