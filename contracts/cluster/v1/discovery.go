// Package clusterv1 defines version 1 of the cluster control-plane contract.
// Its NATS subjects and JSON fields are wire compatibility, not implementation
// details of a daemon or consumer.
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
