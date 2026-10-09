package viperblocklegacyv1

// VolumeAbandonSubject is where the local daemon asks for an export to be torn
// down because the instance using it belongs to another node now.
//
// It exists because an unmount is the wrong request and killing the guest is not
// enough. An unmount seals, and this node's copy is the stale one, so sealing
// would publish an older state over the winner's. Meanwhile the export holds the
// volume lease, and a lease this node keeps renewing is one the winner can never
// acquire — the guest would be stopped here and unable to start anywhere.
//
// Node-addressed, like mount and unmount: the export is local, and no other
// node's viperblockd has anything it could tear down.
func VolumeAbandonSubject(node string) string {
	if node == "" {
		return "ebs.abandon"
	}
	return "ebs." + node + ".abandon"
}

// VolumeAbandonRequest asks for one volume's export to go away without sealing.
// Reason is logged, so a node that dropped an export can be told apart from one
// that was fenced.
type VolumeAbandonRequest struct {
	Volume string `json:"volume"`
	Reason string `json:"reason"`
}

// VolumeAbandonResponse reports what happened. Abandoned is false with no error
// when the volume was not exported here, which is the ordinary case and not a
// failure: the request is idempotent by design, since the caller is reconciling
// rather than transacting.
type VolumeAbandonResponse struct {
	Abandoned bool   `json:"abandoned"`
	Error     string `json:"error,omitempty"`
}
