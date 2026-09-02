package viperblockd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/otelsetup"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// volumeDirtyBucket names the volumes whose writes are not confirmed to have
// reached the backend, so the node holding them is still known after that node
// is gone.
//
// Deliberately no TTL. The volume lease answers "who is writing this right now"
// and expires with its holder; this answers "whose copy is ahead of the
// backend", which has to outlive the holder to be worth anything.
//
// It is a placement input, not a gate. Instance start already prefers the node
// that last ran the instance and falls back after a window, and refusing the
// fallback here would trade a rare data-loss risk for a volume that cannot run
// anywhere.
const volumeDirtyBucket = "VIPERBLOCK_VOLUME_DIRTY"

// volumeDirtyRecord is what a node publishes about writes the backend may not
// have. Reason says how it got that way, so an operator reading a takeover
// warning does not have to correlate journals across nodes.
//
// Generation is the lease revision the writer held. It orders two nodes' claims
// on one volume, which is the whole reason a returning node cannot quietly
// overwrite or clear a marker that has moved on without it.
type volumeDirtyRecord struct {
	Owner      string    `json:"owner"`
	Generation uint64    `json:"generation"`
	Since      time.Time `json:"since"`
	Reason     string    `json:"reason"`
}

// errDirtyMarkerSuperseded reports a write refused because the marker names a
// later generation. The caller holds a lease it no longer owns.
var errDirtyMarkerSuperseded = errors.New("dirty marker belongs to a later generation")

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

// mark records that this node holds writes for volumeName at generation.
// Conditional on the existing entry: a marker written by a later generation
// belongs to a node that has taken the volume over, and overwriting it would
// name this node as the current copy after it stopped being one.
//
// Writing at the same generation is the ordinary case — every open marks, and a
// later failed seal on the same open refines the reason.
func (d *volumeDirty) mark(ctx context.Context, volumeName string, generation uint64, reason string) error {
	if !volumeLeaseKeyPattern.MatchString(volumeName) {
		return fmt.Errorf("volume name %q cannot be a dirty key", volumeName)
	}
	payload, err := json.Marshal(volumeDirtyRecord{
		Owner:      d.owner,
		Generation: generation,
		Since:      time.Now().UTC(),
		Reason:     reason,
	})
	if err != nil {
		return fmt.Errorf("marshal dirty record: %w", err)
	}

	entry, err := d.kv.Get(ctx, volumeName)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		if _, err := d.kv.Create(ctx, volumeName, payload); err != nil {
			return fmt.Errorf("mark %s dirty: %w", volumeName, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read dirty mark on %s: %w", volumeName, err)
	}

	var current volumeDirtyRecord
	if err := json.Unmarshal(entry.Value(), &current); err == nil && current.Generation > generation {
		return fmt.Errorf("%w: %s holds generation %d, this node has %d",
			errDirtyMarkerSuperseded, current.Owner, current.Generation, generation)
	}

	// Conditioned on the revision just read, so a concurrent takeover between
	// the read and the write loses this update rather than overwriting theirs.
	if _, err := d.kv.Update(ctx, volumeName, payload, entry.Revision()); err != nil {
		return fmt.Errorf("mark %s dirty: %w", volumeName, err)
	}
	return nil
}

// clear drops the marker after a seal that reached the backend. Absent is the
// normal case, so a missing key is not an error.
//
// Only clears a marker this node wrote at this generation. A node returning
// after a takeover still has local state to seal, and sealing it says nothing
// about the copy that moved on — clearing the winner's marker there would erase
// the record that the winner's own writes are unconfirmed.
func (d *volumeDirty) clear(ctx context.Context, volumeName string, generation uint64) error {
	entry, err := d.kv.Get(ctx, volumeName)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read dirty mark on %s: %w", volumeName, err)
	}

	var current volumeDirtyRecord
	if err := json.Unmarshal(entry.Value(), &current); err != nil {
		return fmt.Errorf("unreadable dirty mark on %s: %w", volumeName, err)
	}
	if current.Owner != d.owner || current.Generation != generation {
		slog.WarnContext(ctx, "not clearing a dirty marker this node does not own, the volume moved on",
			"volume", volumeName, "marker_owner", current.Owner, "marker_generation", current.Generation,
			"this_owner", d.owner, "this_generation", generation)
		return nil
	}

	if err := d.kv.Delete(ctx, volumeName, jetstream.LastRevision(entry.Revision())); err != nil &&
		!errors.Is(err, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("clear dirty mark on %s: %w", volumeName, err)
	}
	return nil
}

// purge removes the marker for a volume that no longer exists. Unconditional
// because delete is permanent: there is no copy left for an owner to hold, so a
// marker naming one describes nothing.
func (d *volumeDirty) purge(ctx context.Context, volumeName string) error {
	if err := d.kv.Delete(ctx, volumeName); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("purge dirty mark on %s: %w", volumeName, err)
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

// markVolumeDirty records that this node holds writes for volumeName.
//
// The error is the caller's to honour on the open path. A volume opened without
// a marker is one a later takeover cannot warn about, so a node that cannot
// write the marker cannot show it is safe to write the volume.
func (cfg *Config) markVolumeDirty(ctx context.Context, volumeName string, generation uint64, reason string) error {
	if cfg.dirty == nil {
		return nil
	}
	if err := cfg.dirty.mark(ctx, volumeName, generation, reason); err != nil {
		slog.ErrorContext(ctx, "could not record that this node holds unconfirmed writes",
			"volume", volumeName, "generation", generation, "err", err)
		return err
	}
	// Every open marks, so this is the routine case. The events worth a human's
	// attention — a failed seal, a takeover — are logged by their own callers.
	slog.DebugContext(ctx, "volume writes not yet confirmed to the backend",
		"volume", volumeName, "owner", cfg.dirty.owner, "generation", generation, "reason", reason)
	return nil
}

// markVolumeDirtyAfterFailedSeal records a seal that did not reach the backend.
// Best effort, unlike the open path: the seal has already failed and the
// caller's own error is the one that matters.
func (cfg *Config) markVolumeDirtyAfterFailedSeal(ctx context.Context, volumeName string, generation uint64, reason string) {
	if err := cfg.markVolumeDirty(ctx, volumeName, generation, reason); err != nil {
		slog.ErrorContext(ctx, "could not record the failed seal, so a start elsewhere will not warn",
			"volume", volumeName, "err", err)
	}
}

// clearVolumeDirty drops the marker after a successful seal, if this node still
// owns it at this generation.
func (cfg *Config) clearVolumeDirty(ctx context.Context, volumeName string, generation uint64) {
	if cfg.dirty == nil {
		return
	}
	if err := cfg.dirty.clear(ctx, volumeName, generation); err != nil {
		slog.ErrorContext(ctx, "could not clear the dirty mark, so later starts stay marked as unconfirmed",
			"volume", volumeName, "err", err)
	}
}

// purgeVolumeDirty drops the marker for a deleted volume.
func (cfg *Config) purgeVolumeDirty(ctx context.Context, volumeName string) {
	if cfg.dirty == nil {
		return
	}
	if err := cfg.dirty.purge(ctx, volumeName); err != nil {
		slog.ErrorContext(ctx, "could not remove the dirty mark for a deleted volume",
			"volume", volumeName, "err", err)
	}
}

// reportVolumeTakeover records that this node is opening a volume another node
// holds unsealed writes for, and takes ownership of the marker.
//
// It never refuses. Instance start forwards to the node that last ran the
// instance and only falls back here once that node has failed its window, so by
// the time this runs the alternative is not "wait for the better copy", it is
// "the instance runs nowhere". The warning is the deliverable: this is the one
// moment a human can be told which writes are about to be left behind.
// It reports whether it took the marker over, so the caller does not then
// overwrite the detail with a generic one.
func (cfg *Config) reportVolumeTakeover(ctx context.Context, volumeName string, generation uint64) (bool, error) {
	if cfg.dirty == nil {
		return false, nil
	}
	record, ok := cfg.dirty.holder(ctx, volumeName)
	if !ok || record.Owner == "" || record.Owner == cfg.dirty.owner {
		return false, nil
	}

	slog.WarnContext(ctx, "opening a volume whose writes were last held by another node, from the backend checkpoint instead",
		"volume", volumeName, "previous_owner", record.Owner,
		"unsealed_since", record.Since.Format(time.RFC3339), "previous_reason", record.Reason)
	otelsetup.RecordVolumeTakeover(ctx)

	// Take the marker over. The previous owner's copy is now behind this one,
	// so if that node returns it must not be treated as the better source.
	reason := fmt.Sprintf("took over from %s, which held unsealed writes since %s (%s)",
		record.Owner, record.Since.Format(time.RFC3339), record.Reason)
	return true, cfg.markVolumeDirty(ctx, volumeName, generation, reason)
}

// UnsealedVolume reports a volume whose writes are not confirmed to have
// reached the backend, and which node holds them.
type UnsealedVolume struct {
	VolumeID string
	Owner    string
	Since    time.Time
	Reason   string
}

// bindDirtyBucket opens the dirty bucket read-only for an operator tool. It does
// not create the bucket: a cluster where no volume has ever been opened has no
// bucket, and reporting that as an error would read as a fault.
func bindDirtyBucket(ctx context.Context, nc *nats.Conn) (jetstream.KeyValue, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	kv, err := js.KeyValue(ctx, volumeDirtyBucket)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("volume dirty bucket: %w", err)
	}
	return kv, nil
}

// ListUnsealedVolumes reports every volume a node holds writes for that the
// backend may not have.
//
// Starting one elsewhere is allowed and preferred over an instance that cannot
// run, but it opens from the last checkpoint that did reach the backend. This
// is the list of volumes where that trade would cost something.
func ListUnsealedVolumes(ctx context.Context, nc *nats.Conn) ([]UnsealedVolume, error) {
	kv, err := bindDirtyBucket(ctx, nc)
	if err != nil || kv == nil {
		return nil, err
	}
	keys, err := kv.Keys(ctx)
	if errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list unsealed volumes: %w", err)
	}

	unsealed := make([]UnsealedVolume, 0, len(keys))
	for _, key := range keys {
		entry, err := kv.Get(ctx, key)
		if err != nil {
			continue
		}
		var record volumeDirtyRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			// Something holds writes for this volume even if the record cannot
			// be read, so report it rather than hide it.
			unsealed = append(unsealed, UnsealedVolume{VolumeID: key, Reason: "marker is unreadable"})
			continue
		}
		unsealed = append(unsealed, UnsealedVolume{
			VolumeID: key, Owner: record.Owner, Since: record.Since, Reason: record.Reason,
		})
	}
	return unsealed, nil
}
