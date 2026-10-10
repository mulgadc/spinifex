package viperblocklegacyv1

import (
	"regexp"
	"time"
)

// DirtyBucket names the volumes whose writes are not confirmed to have reached
// the backend, so the node holding them is still known after that node is gone.
//
// Deliberately no TTL. The volume lease answers "who is writing this right now"
// and expires with its holder; this answers "whose copy is ahead of the
// backend", which has to outlive the holder to be worth anything.
//
// It is a placement input, not a gate. Instance start already prefers the node
// that last ran the instance and falls back after a window, and refusing the
// fallback here would trade a rare data-loss risk for a volume that cannot run
// anywhere.
const DirtyBucket = "VIPERBLOCK_VOLUME_DIRTY"

// VolumeKeyPattern is what JetStream KV accepts as a key for a volume-keyed
// bucket, including the dirty marker and the volume lease. Volume names reach
// here from the wire, and a name carrying "." or ">" would address somebody
// else's key.
var VolumeKeyPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// DirtyRecord is what a node publishes about writes the backend may not have.
// Reason says how it got that way, so an operator reading a takeover warning
// does not have to correlate journals across nodes.
//
// Generation is the lease revision the writer held. It orders two nodes' claims
// on one volume, which is the whole reason a returning node cannot quietly
// overwrite or clear a marker that has moved on without it.
type DirtyRecord struct {
	Owner      string    `json:"owner"`
	Generation uint64    `json:"generation"`
	Since      time.Time `json:"since"`
	Reason     string    `json:"reason"`
}
