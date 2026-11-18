package projection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/foundation/release/featurelevel"
	"github.com/nats-io/nats.go"
)

// EndpointListenerPort is the gate's endpoint listener port every node's
// Predastore serves, which a node's endpoint proxy dials over mutual TLS.
const EndpointListenerPort = 8474

// EndpointProxyStatusSubject is the subject a node's vpcd answers its endpoint
// proxy status on. The reply names the node, and a reply naming another is refused.
func EndpointProxyStatusSubject(node string) string {
	return "vpcd.v1.EndpointProxyStatus." + node
}

// EndpointProxyStatusRequest asks node's vpcd for its endpoint proxy status.
// A vpcd refuses a request naming another node.
type EndpointProxyStatusRequest struct {
	Node string `json:"node"`
}

// EndpointProxyStatus is vpcd's answer: the feature-level revision read before
// its last published view, the endpoints that view admits by, its connections,
// its capture probe and its own gate's mutual TLS. A refusal carries only Error.
type EndpointProxyStatus struct {
	Node        string `json:"node"`
	Revision    uint64 `json:"revision"`
	Sessions    int    `json:"sessions"`
	Endpoints   int    `json:"endpoints"`
	ProbePassed bool   `json:"probe_passed"`
	GateReady   bool   `json:"gate_ready"`
	Error       string `json:"error,omitempty"`
}

// RequestEndpointProxyStatus asks node's vpcd for its endpoint proxy status. A
// refusal, a malformed reply or a reply naming another node is an error.
func RequestEndpointProxyStatus(ctx context.Context, nc *nats.Conn, node string) (EndpointProxyStatus, error) {
	var st EndpointProxyStatus
	if nc == nil {
		return st, errors.New("no NATS connection")
	}
	if !featurelevel.ValidNode(node) {
		return st, errors.New("invalid node ID")
	}
	body, err := json.Marshal(EndpointProxyStatusRequest{Node: node})
	if err != nil {
		return st, err
	}
	msg, err := nc.RequestWithContext(ctx, EndpointProxyStatusSubject(node), body)
	if err != nil {
		return st, fmt.Errorf("no reply: %w", err)
	}
	if err := json.Unmarshal(msg.Data, &st); err != nil {
		return st, fmt.Errorf("malformed reply: %w", err)
	}
	switch {
	case st.Error != "":
		return st, fmt.Errorf("refused: %s", st.Error)
	case st.Node != node:
		return st, fmt.Errorf("answered by %q", st.Node)
	}
	return st, nil
}
