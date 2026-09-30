// Package clusterv1 defines version 1 of the cross-process cluster-membership
// contract. These messages establish which daemon nodes are responding to the
// control plane; their subject and JSON fields are wire compatibility.
package clusterv1

const (
	// NodesDiscoverSubject fans out a membership discovery request to every
	// active daemon. Its reply payload is NodeDiscoverResponse.
	NodesDiscoverSubject = "spinifex.nodes.discover"
)

// NodeDiscoverResponse identifies one daemon that responded to a membership
// discovery request.
type NodeDiscoverResponse struct {
	Node string `json:"node"`
}
