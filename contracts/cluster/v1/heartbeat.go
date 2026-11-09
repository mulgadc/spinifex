package clusterv1

import (
	"encoding/json"
	"time"
)

// ClusterStateBucket is the KV bucket holding each daemon's heartbeat alongside
// other cluster-wide state.
const ClusterStateBucket = "spinifex-cluster-state"

// HeartbeatKey is the ClusterStateBucket key a node writes its heartbeat under.
func HeartbeatKey(node string) string {
	return "heartbeat." + node
}

// LivenessWindow is how recently a node must have heartbeated for a reader
// deciding on a takeover to count it live. It is six heartbeats, compared
// across node clocks, where NTP skew is small against it.
const LivenessWindow = 60 * time.Second

// LiveAt reports whether a heartbeat written at written still vouches for its
// node at now: it does while it is under LivenessWindow old.
func LiveAt(written, now time.Time) bool {
	return now.Sub(written) < LivenessWindow
}

// HeartbeatWritten dates a stored heartbeat value. One that cannot be decoded
// or dated vouches for nothing, which is an answer rather than a fault.
func HeartbeatWritten(data []byte) (time.Time, bool) {
	var beat HeartbeatTimestamp
	if json.Unmarshal(data, &beat) != nil {
		return time.Time{}, false
	}
	return beat.Time()
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
