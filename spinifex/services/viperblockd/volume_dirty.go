package viperblockd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// volumeDirtyBucket names the volumes whose last seal failed, so the node
// holding the only current copy is still known after that node is gone.
//
// Deliberately no TTL. The volume lease already answers "who is writing this
// right now" and expires with its holder, which is exactly the property
// durability cannot be built on: a node that dies holding un-uploaded writes
// stops renewing, and 45 seconds later nothing refuses a stale start elsewhere.
const volumeDirtyBucket = "VIPERBLOCK_VOLUME_DIRTY"

// errVolumeDirtyElsewhere reports that the volume's current data is on another
// node. Distinguishable so a caller can name the node rather than fail blankly.
var errVolumeDirtyElsewhere = errors.New("volume was last written on another node")

// volumeDirtyRecord is what a node publishes about a seal it could not
// complete. Reason carries the seal error so an operator sees why the volume
// is pinned without correlating journals across nodes.
type volumeDirtyRecord struct {
	Owner  string    `json:"owner"`
	Since  time.Time `json:"since"`
	Reason string    `json:"reason"`
}

// volumeDirty records and answers "which node holds this volume's only current
// copy", from a JetStream KV bucket that outlives the node.
type volumeDirty struct {
	kv    jetstream.KeyValue
	owner string
}

// newVolumeDirty binds the dirty bucket, creating it if this is the first node
// up. owner identifies this node in the entries it writes.
func newVolumeDirty(ctx context.Context, nc *nats.Conn, owner string) (*volumeDirty, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      volumeDirtyBucket,
		Description: "volumes whose last seal failed, keyed by volume, naming the node holding the current copy",
		History:     1,
	})
	if err != nil {
		return nil, fmt.Errorf("volume dirty bucket: %w", err)
	}
	return &volumeDirty{kv: kv, owner: owner}, nil
}

// mark records that volumeName's seal failed on this node. Overwrites any
// existing entry: a later failure on the same node is the current truth, and an
// entry naming another node cannot exist for a volume this node just sealed.
func (d *volumeDirty) mark(ctx context.Context, volumeName, reason string) error {
	if !volumeLeaseKeyPattern.MatchString(volumeName) {
		return fmt.Errorf("volume name %q cannot be a dirty key", volumeName)
	}
	payload, err := json.Marshal(volumeDirtyRecord{
		Owner:  d.owner,
		Since:  time.Now().UTC(),
		Reason: reason,
	})
	if err != nil {
		return fmt.Errorf("marshal dirty record: %w", err)
	}
	if _, err := d.kv.Put(ctx, volumeName, payload); err != nil {
		return fmt.Errorf("mark %s dirty: %w", volumeName, err)
	}
	return nil
}

// clear drops the marker after a seal that reached the backend. Absent is the
// normal case, so a missing key is not an error.
func (d *volumeDirty) clear(ctx context.Context, volumeName string) error {
	if err := d.kv.Delete(ctx, volumeName); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("clear dirty mark on %s: %w", volumeName, err)
	}
	return nil
}

// holder returns the record for volumeName and whether one exists. An entry
// that cannot be read is reported as absent: refusing every mount because the
// bucket is unreadable would convert a degraded cluster into a stopped one.
func (d *volumeDirty) holder(ctx context.Context, volumeName string) (volumeDirtyRecord, bool) {
	entry, err := d.kv.Get(ctx, volumeName)
	if err != nil {
		if !errors.Is(err, jetstream.ErrKeyNotFound) {
			slog.Warn("volume dirty: could not read marker", "volume", volumeName, "err", err)
		}
		return volumeDirtyRecord{}, false
	}
	var record volumeDirtyRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		slog.Warn("volume dirty: unreadable marker", "volume", volumeName, "err", err)
		return volumeDirtyRecord{}, false
	}
	return record, true
}

// markVolumeDirty records a failed seal. Best effort by design: the seal has
// already failed, and the caller's own error is the one that matters.
func (cfg *Config) markVolumeDirty(ctx context.Context, volumeName, reason string) {
	if cfg.dirty == nil {
		return
	}
	if err := cfg.dirty.mark(ctx, volumeName, reason); err != nil {
		slog.ErrorContext(ctx, "could not record the failed seal, so a start elsewhere will not be refused",
			"volume", volumeName, "err", err)
		return
	}
	slog.WarnContext(ctx, "volume marked dirty: its only current copy is on this node",
		"volume", volumeName, "owner", cfg.dirty.owner, "reason", reason)
}

// clearVolumeDirty drops the marker after a successful seal.
func (cfg *Config) clearVolumeDirty(ctx context.Context, volumeName string) {
	if cfg.dirty == nil {
		return
	}
	if err := cfg.dirty.clear(ctx, volumeName); err != nil {
		slog.ErrorContext(ctx, "could not clear the dirty mark, so later starts elsewhere stay refused",
			"volume", volumeName, "err", err)
	}
}

// checkVolumeDirty refuses a mount of a volume whose current data is on another
// node. This is the durable half of the exclusion: the lease refuses a live
// remote holder and expires with it, and this refuses a dead one indefinitely.
func (cfg *Config) checkVolumeDirty(ctx context.Context, volumeName string) error {
	if cfg.dirty == nil {
		return nil
	}
	record, ok := cfg.dirty.holder(ctx, volumeName)
	if !ok || record.Owner == "" || record.Owner == cfg.dirty.owner {
		return nil
	}
	return fmt.Errorf("%w: %s holds it since %s (last seal failed: %s)",
		errVolumeDirtyElsewhere, record.Owner, record.Since.Format(time.RFC3339), record.Reason)
}
