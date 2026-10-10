package viperblocklegacyv1

// VolumeFencedSubject is where a fenced volume is announced so the daemon on
// this node can stop the guest that was using it. Node-addressed: the guest is
// local, and every other node's daemon has nothing to do about it.
func VolumeFencedSubject(node string) string {
	if node == "" {
		return "ebs.fenced"
	}
	return "ebs." + node + ".fenced"
}

// VolumeFencedEvent announces that a node tore down a volume's export because
// the lease moved. Winner is who holds it now, for the operator reading the
// guest's stop reason.
type VolumeFencedEvent struct {
	Volume string `json:"volume"`
	Node   string `json:"node"`
	Winner string `json:"winner"`
	Reason string `json:"reason"`
}
