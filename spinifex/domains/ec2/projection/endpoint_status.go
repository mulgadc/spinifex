package projection

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

// EndpointProxyStatus is vpcd's answer: the feature-level revision it last saw,
// its open sessions, whether the capture probe passed and whether its own gate
// accepted the proxy's mutual TLS. A refusal carries only Error.
type EndpointProxyStatus struct {
	Node        string `json:"node"`
	Revision    uint64 `json:"revision"`
	Sessions    int    `json:"sessions"`
	ProbePassed bool   `json:"probe_passed"`
	GateReady   bool   `json:"gate_ready"`
	Error       string `json:"error,omitempty"`
}
