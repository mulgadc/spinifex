package projection

// EndpointProxyStatusSubject is the subject a node's vpcd answers its endpoint
// proxy status on. The reply names the node, and a reply naming another is refused.
func EndpointProxyStatusSubject(node string) string {
	return "vpcd.v1.EndpointProxyStatus." + node
}

// EndpointListenerPort is the gate's endpoint listener, which every node's
// Predastore serves to endpoint proxies.
const EndpointListenerPort = 8474

// EndpointProxyStatusRequest asks Node's vpcd for its status.
type EndpointProxyStatusRequest struct {
	Node string `json:"node"`
}

// EndpointProxyStatus is vpcd's answer: the feature-level record revision it
// last observed, its open proxied sessions, whether this kernel passed the
// capture probe, and whether this node's gate serves the endpoint listener.
// Error alone is set when vpcd refuses the request.
type EndpointProxyStatus struct {
	Node        string `json:"node"`
	Revision    uint64 `json:"revision"`
	Sessions    int    `json:"sessions"`
	ProbePassed bool   `json:"probe_passed"`
	GateReady   bool   `json:"gate_ready"`
	Error       string `json:"error,omitempty"`
}
