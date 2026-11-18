package projection

// EndpointProxyStatusSubject is the subject a node's vpcd answers its endpoint
// proxy status on. The reply names the node, and a reply naming another is refused.
func EndpointProxyStatusSubject(node string) string {
	return "vpcd.v1.EndpointProxyStatus." + node
}

// EndpointProxyStatus is vpcd's answer: the feature-level record revision it
// last observed, its open proxied sessions, and whether this kernel passed the
// capture probe. The rollback drain and the node's advertised level read it.
type EndpointProxyStatus struct {
	Node        string `json:"node"`
	Revision    uint64 `json:"revision"`
	Sessions    int    `json:"sessions"`
	ProbePassed bool   `json:"probe_passed"`
}
