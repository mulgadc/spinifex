package clusterv1

import "time"

// ClusterStateBucket is the KV bucket holding each daemon's heartbeat alongside
// other cluster-wide state.
const ClusterStateBucket = "spinifex-cluster-state"

// HeartbeatKey is the ClusterStateBucket key a node writes its heartbeat under.
func HeartbeatKey(node string) string {
	return "heartbeat." + node
}

// HeartbeatTimestamp is the read side of a heartbeat for node liveness. It
// decodes only the timestamp, so a reader does not depend on the capacity
// figures the daemon writes beside it.
type HeartbeatTimestamp struct {
	Timestamp string `json:"timestamp"`
}

// Time returns when the heartbeat was written. A timestamp that is missing or
// unparseable vouches for nothing, so ok is false.
func (h HeartbeatTimestamp) Time() (written time.Time, ok bool) {
	parsed, err := time.Parse(time.RFC3339, h.Timestamp)
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}
