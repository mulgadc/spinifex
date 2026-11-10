package snapshot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	otelsetup "github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
)

// PendingSnapshotReaper settles every pending snapshot document from the
// provider. CreateSnapshot returns once the provider answers, and a snapshot
// that settles later, or a request that crashed or failed ambiguously, leaves
// its document pending; this sweep is where that work converges (ADR-0003:S8).
//
// It deletes nothing a customer did not ask to delete (ADR-0003:S7): only a
// snapshot whose document DeleteSnapshot marked, its reference and document,
// and a late-accepted snapshot under the tombstone that delete left.
type PendingSnapshotReaper struct {
	svc *SnapshotServiceImpl
}

// tombstoneRetention is how long a deleted AmbiguousCreate snapshot's document
// stays, so a create accepted that late is still found and deleted.
const tombstoneRetention = 7 * 24 * time.Hour

// AMI states the reaper moves a CreateImage's AMI between.
const (
	amiStatePending   = "pending"
	amiStateAvailable = ebsmetadata.AMIStateAvailable
	amiStateFailed    = "failed"
)

// reapStep is a point in the reaper's work on one document, for a test to
// interrupt it at.
type reapStep int

const (
	reapReread reapStep = iota + 1
	reapProviderDeleted
	reapRefRemoved
)

// NewPendingSnapshotReaper builds the reaper over this service's documents,
// provider and claims.
func (s *SnapshotServiceImpl) NewPendingSnapshotReaper() *PendingSnapshotReaper {
	return &PendingSnapshotReaper{svc: s}
}

func (r *PendingSnapshotReaper) Class() string { return "pending-snapshot" }

// Sweep reads every snapshot document and settles each pending one it can,
// carries out each accepted delete, and rereads each tombstone; then it settles
// each pending AMI from its snapshot. It reports how many documents it wrote.
func (r *PendingSnapshotReaper) Sweep(ctx context.Context) (int, error) {
	if r.svc.provider == nil {
		return 0, nil
	}
	docs, err := r.svc.metadata.ListAllSnapshotsStrict(ctx)
	if err != nil {
		return 0, fmt.Errorf("list snapshot documents: %w", err)
	}
	written := 0
	for _, doc := range docs {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		var wrote bool
		switch {
		case doc.Tombstoned():
			wrote = r.sweepTombstone(ctx, doc)
		case doc.DeleteRequested, doc.State == snapshotStatePending:
			wrote = r.settle(ctx, doc)
		default:
			continue
		}
		if wrote {
			written++
		}
	}
	amis, err := r.settleAMIs(ctx)
	return written + amis, err
}

// interrupted reports a test stopping the reaper at step for snapshotID.
func (r *PendingSnapshotReaper) interrupted(step reapStep, snapshotID string) bool {
	return r.svc.reapAt != nil && r.svc.reapAt(step, snapshotID)
}

// providerSnapshot reads snapshotID from the provider. found is false when the
// provider holds no snapshot by that ID.
func (r *PendingSnapshotReaper) providerSnapshot(ctx context.Context, doc ebsmetadata.Snapshot) (ebsprovider.SnapshotRef, bool, error) {
	resp, err := r.svc.provider.ListSnapshots(ctx, ebsprovider.ListSnapshotsRequest{
		Versioned: ebsprovider.NewLatestVersioned(), SnapshotID: doc.SnapshotID,
	})
	if err != nil {
		slog.WarnContext(ctx, "pending-snapshot: provider read failed, retrying next sweep",
			"snapshotId", doc.SnapshotID, "accountId", doc.OwnerID, "err", err)
		return ebsprovider.SnapshotRef{}, false, err
	}
	if resp == nil || len(resp.Snapshots) == 0 {
		return ebsprovider.SnapshotRef{}, false, nil
	}
	return resp.Snapshots[0], true, nil
}

// settle follows the provider's state for doc, carries out its accepted
// delete, or repeats its CreateSnapshot when the provider holds no snapshot by
// that ID. It reports whether it wrote.
func (r *PendingSnapshotReaper) settle(ctx context.Context, doc ebsmetadata.Snapshot) bool {
	// A marked document is only cleaned up, from a provider read under its claim.
	if doc.DeleteRequested {
		return r.underClaim(ctx, doc, nil)
	}
	ref, found, err := r.providerSnapshot(ctx, doc)
	if err != nil {
		return false
	}
	if found {
		return r.follow(ctx, doc, ref)
	}
	return r.underClaim(ctx, doc, func(current ebsmetadata.Snapshot) bool { return r.repeat(ctx, current) })
}

// providerSettled reports a provider snapshot that is completed or errored,
// the states in which a provider deletes it.
func providerSettled(ref ebsprovider.SnapshotRef) bool {
	return ref.State == ebsprovider.SnapshotStateCompleted || ref.State == ebsprovider.SnapshotStateError
}

// withClaim runs fn under id's claim, which take acquires, and reports false
// without running it when the claim is not taken.
func withClaim(ctx context.Context, id string, take func(context.Context, string) (*heldClaim, error), fn func() bool) bool {
	held, err := take(ctx, id)
	if err != nil {
		level := slog.LevelWarn
		if errors.Is(err, errClaimHeld) {
			level = slog.LevelInfo
		}
		slog.Log(ctx, level, "pending-snapshot: claim not taken, left for the next sweep", "id", id, "err", err)
		return false
	}
	defer held.release(ctx)
	return fn()
}

// underClaim takes doc's claim, rereads its document and runs act on it while
// it is unmarked and pending. A marked document, however it was read before,
// is cleaned up instead.
func (r *PendingSnapshotReaper) underClaim(ctx context.Context, doc ebsmetadata.Snapshot, act func(ebsmetadata.Snapshot) bool) bool {
	return withClaim(ctx, doc.SnapshotID, r.svc.claims.take, func() bool {
		// The claim may have been taken after the creating request rolled back or
		// settled the document, so it is read again before anything is written.
		current, err := r.svc.metadata.GetSnapshot(ctx, doc.OwnerID, doc.SnapshotID)
		if err != nil || current.Tombstoned() {
			return false
		}
		if r.interrupted(reapReread, doc.SnapshotID) {
			return false
		}
		if current.DeleteRequested {
			return r.cleanUpIfSettled(ctx, current)
		}
		if current.State != snapshotStatePending {
			return false
		}
		return act(current)
	})
}

// cleanUpIfSettled reads doc's snapshot from the provider and carries out its
// accepted delete once the provider no longer holds it pending.
func (r *PendingSnapshotReaper) cleanUpIfSettled(ctx context.Context, doc ebsmetadata.Snapshot) bool {
	ref, found, err := r.providerSnapshot(ctx, doc)
	if err != nil || (found && !providerSettled(ref)) {
		return false
	}
	return r.cleanUp(ctx, doc, ref, found)
}

// deleteAtProvider deletes snapshotID at the provider; one already gone there
// is deleted.
func (r *PendingSnapshotReaper) deleteAtProvider(ctx context.Context, snapshotID, handle string) error {
	err := r.svc.provider.DeleteSnapshot(ctx, ebsprovider.DeleteSnapshotRequest{
		Versioned: ebsprovider.NewVersioned(), SnapshotID: snapshotID, Handle: handle,
	})
	if errors.Is(err, ebsprovider.ErrNotFound) {
		return nil
	}
	return err
}

// follow writes the state the provider reports for a snapshot it holds.
func (r *PendingSnapshotReaper) follow(ctx context.Context, doc ebsmetadata.Snapshot, ref ebsprovider.SnapshotRef) bool {
	var mutate func(*ebsmetadata.Snapshot)
	switch ref.State {
	case ebsprovider.SnapshotStateCompleted:
		mutate = func(d *ebsmetadata.Snapshot) {
			markSnapshotCompleted(d)
			if d.ProviderHandle == "" {
				d.ProviderHandle = ref.Handle
			}
		}
	case ebsprovider.SnapshotStateError:
		mutate = markSnapshotError
	case ebsprovider.SnapshotStatePending:
		return false
	default:
		slog.WarnContext(ctx, "pending-snapshot: provider reported no state for a held snapshot, retrying next sweep",
			"snapshotId", doc.SnapshotID, "state", ref.State)
		return false
	}
	return r.underClaim(ctx, doc, func(current ebsmetadata.Snapshot) bool { return r.write(ctx, current, mutate) })
}

// repeat asks the provider again, under the snapshot's claim, for a pending
// snapshot it does not hold. Absence alone settles nothing, because a call the
// creating request sent may still be accepted; the repeat returns that snapshot or makes it.
func (r *PendingSnapshotReaper) repeat(ctx context.Context, current ebsmetadata.Snapshot) bool {
	handle, ok := r.volumeHandle(ctx, current)
	if !ok {
		return false
	}
	// The flag goes on before the call, so a crash once it is sent leaves it.
	earlierUnanswered := current.AmbiguousCreate
	if !earlierUnanswered {
		if _, ok, err := r.svc.updatePending(ctx, current.OwnerID, current.SnapshotID, func(d *ebsmetadata.Snapshot) {
			d.AmbiguousCreate = true
		}); err != nil || !ok {
			slog.WarnContext(ctx, "pending-snapshot: could not record the repeat before sending it, retrying next sweep",
				"snapshotId", current.SnapshotID, "err", err)
			return false
		}
	}
	created, err := r.svc.provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{
		Versioned: ebsprovider.NewVersioned(), SnapshotID: current.SnapshotID, VolumeID: current.VolumeID, VolumeHandle: handle,
	})
	switch {
	case err == nil && created != nil:
		return r.write(ctx, current, applyProviderAnswer(created, earlierUnanswered))
	case refusalSettlesError(err):
		slog.WarnContext(ctx, "pending-snapshot: provider refused the repeat for good, marking the snapshot error",
			"snapshotId", current.SnapshotID, "volumeId", current.VolumeID, "err", err)
		return r.write(ctx, current, func(d *ebsmetadata.Snapshot) {
			markSnapshotError(d)
			d.AmbiguousCreate = earlierUnanswered
		})
	default:
		slog.WarnContext(ctx, "pending-snapshot: repeat not answered, retrying next sweep",
			"snapshotId", current.SnapshotID, "volumeId", current.VolumeID, "err", err)
		return false
	}
}

// cleanUp carries out an accepted delete, under the snapshot's claim: the
// provider's delete, then the reference, then the document, or a tombstone if
// a create on it went unanswered. Each step must succeed before the next.
func (r *PendingSnapshotReaper) cleanUp(ctx context.Context, doc ebsmetadata.Snapshot, ref ebsprovider.SnapshotRef, found bool) bool {
	// A marked snapshot the provider does not hold is never repeated: the
	// customer asked for it gone, and a tombstone catches a late acceptance.
	if found {
		handle := ref.Handle
		if handle == "" {
			handle = doc.ProviderHandle
		}
		if err := r.deleteAtProvider(ctx, doc.SnapshotID, handle); err != nil {
			slog.WarnContext(ctx, "pending-snapshot: provider delete of a deleted snapshot failed, retrying next sweep",
				"snapshotId", doc.SnapshotID, "err", err)
			return false
		}
		if r.interrupted(reapProviderDeleted, doc.SnapshotID) {
			return false
		}
	}
	// The document goes last: it is the only record this sweep discovers work
	// from, and a reference left without it would block DeleteVolume for good.
	if err := r.svc.removeSnapshotRef(ctx, doc.VolumeID, doc.SnapshotID); err != nil {
		slog.WarnContext(ctx, "pending-snapshot: reference of a deleted snapshot not removed, retrying next sweep",
			"snapshotId", doc.SnapshotID, "volumeId", doc.VolumeID, "err", err)
		return false
	}
	if r.interrupted(reapRefRemoved, doc.SnapshotID) {
		return false
	}
	if doc.AmbiguousCreate {
		doc.TombstonedAt = r.svc.clock().UTC()
		if err := r.svc.metadata.PutSnapshot(ctx, doc); err != nil {
			slog.WarnContext(ctx, "pending-snapshot: tombstone of a deleted snapshot not written, retrying next sweep",
				"snapshotId", doc.SnapshotID, "err", err)
			return false
		}
		slog.InfoContext(ctx, "pending-snapshot: deleted, keeping a tombstone for a create that may still be accepted",
			"snapshotId", doc.SnapshotID)
		return true
	}
	if err := r.svc.metadata.DeleteSnapshot(ctx, doc.OwnerID, doc.SnapshotID); err != nil && !objectstore.IsNoSuchKeyError(err) {
		slog.WarnContext(ctx, "pending-snapshot: document of a deleted snapshot not removed, retrying next sweep",
			"snapshotId", doc.SnapshotID, "err", err)
		return false
	}
	slog.InfoContext(ctx, "pending-snapshot: deleted", "snapshotId", doc.SnapshotID)
	return true
}

// sweepTombstone reads the provider for a tombstone's ID and deletes a snapshot a late
// create made there once it is no longer pending. A tombstone past its
// retention, with nothing pending under it, is dropped.
func (r *PendingSnapshotReaper) sweepTombstone(ctx context.Context, doc ebsmetadata.Snapshot) bool {
	return withClaim(ctx, doc.SnapshotID, r.svc.claims.take, func() bool {
		ref, found, err := r.providerSnapshot(ctx, doc)
		if err != nil {
			return false
		}
		if found {
			if !providerSettled(ref) {
				return false
			}
			if err := r.deleteAtProvider(ctx, doc.SnapshotID, ref.Handle); err != nil {
				slog.WarnContext(ctx, "pending-snapshot: late-accepted snapshot of a deleted ID not deleted, retrying next sweep",
					"snapshotId", doc.SnapshotID, "err", err)
				return false
			}
			slog.WarnContext(ctx, "pending-snapshot: deleted a snapshot accepted after the customer deleted it",
				"snapshotId", doc.SnapshotID)
		}
		age := r.svc.clock().Sub(doc.TombstonedAt)
		if age < tombstoneRetention {
			return found
		}
		if err := r.svc.metadata.DeleteSnapshot(ctx, doc.OwnerID, doc.SnapshotID); err != nil && !objectstore.IsNoSuchKeyError(err) {
			slog.WarnContext(ctx, "pending-snapshot: expired tombstone not dropped, retrying next sweep",
				"snapshotId", doc.SnapshotID, "err", err)
			return found
		}
		slog.InfoContext(ctx, "pending-snapshot: dropped an expired tombstone", "snapshotId", doc.SnapshotID, "age_ms", otelsetup.Millis(age))
		return true
	})
}

// settleAMIs moves each pending AMI to its snapshot's state: completed makes it
// available, and error or a missing or deleted snapshot makes it failed.
func (r *PendingSnapshotReaper) settleAMIs(ctx context.Context) (int, error) {
	amis, err := r.svc.metadata.ListAMIs(ctx)
	if err != nil {
		return 0, fmt.Errorf("list AMI documents: %w", err)
	}
	written := 0
	for _, ami := range amis {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		if ami.State != amiStatePending || ami.SnapshotID == "" {
			continue
		}
		state, ok := r.amiStateFromSnapshot(ctx, ami)
		if !ok {
			continue
		}
		if r.svc.settlePendingAMI(ctx, ami.ImageID, state, r.svc.claims.take) {
			written++
		}
	}
	return written, nil
}

// amiStateFromSnapshot is the state ami's snapshot document gives it, and
// false while that snapshot is pending or cannot be read.
func (r *PendingSnapshotReaper) amiStateFromSnapshot(ctx context.Context, ami ebsmetadata.AMI) (string, bool) {
	owner := awsidentifiers.GlobalAccountID
	if awsidentifiers.IsAccountID(ami.ImageOwnerAlias) {
		owner = ami.ImageOwnerAlias
	}
	snap, err := r.svc.metadata.GetSnapshot(ctx, owner, ami.SnapshotID)
	switch {
	case objectstore.IsNoSuchKeyError(err):
		return amiStateFailed, true
	case err != nil:
		slog.WarnContext(ctx, "pending-snapshot: snapshot of a pending AMI unreadable, retrying next sweep",
			"imageId", ami.ImageID, "snapshotId", ami.SnapshotID, "err", err)
		return "", false
	case snap.Hidden() || snap.State == snapshotStateError:
		return amiStateFailed, true
	case snap.State == snapshotStateCompleted:
		return amiStateAvailable, true
	default:
		return "", false
	}
}

// volumeHandle is the provider handle of doc's source volume, which the
// repeat sends as the request did. A volume document that is gone gives no
// handle, and the provider answers for the volume; one unreadable is retried.
func (r *PendingSnapshotReaper) volumeHandle(ctx context.Context, doc ebsmetadata.Snapshot) (string, bool) {
	volume, err := r.svc.metadata.GetVolume(ctx, doc.OwnerID, doc.VolumeID)
	if err == nil {
		return volume.ProviderHandle, true
	}
	if objectstore.IsNoSuchKeyError(err) {
		return "", true
	}
	slog.WarnContext(ctx, "pending-snapshot: source volume document unreadable, retrying next sweep",
		"snapshotId", doc.SnapshotID, "volumeId", doc.VolumeID, "err", err)
	return "", false
}

// write applies mutate to doc's current document while it is still pending,
// under a claim the caller holds. A request that crashed between its document
// and its reference left the reference missing, so it is restored first.
func (r *PendingSnapshotReaper) write(ctx context.Context, doc ebsmetadata.Snapshot, mutate func(*ebsmetadata.Snapshot)) bool {
	if err := r.svc.ensureSnapshotRef(ctx, doc.VolumeID, doc.SnapshotID); err != nil {
		slog.WarnContext(ctx, "pending-snapshot: volume reference not restored, retrying next sweep",
			"snapshotId", doc.SnapshotID, "volumeId", doc.VolumeID, "err", err)
		return false
	}
	defer r.dropRefIfDeleted(ctx, doc)
	written, ok, err := r.svc.updatePending(ctx, doc.OwnerID, doc.SnapshotID, mutate)
	if err != nil {
		slog.WarnContext(ctx, "pending-snapshot: document write failed, retrying next sweep",
			"snapshotId", doc.SnapshotID, "accountId", doc.OwnerID, "err", err)
		return false
	}
	if ok && written.State != snapshotStatePending {
		slog.InfoContext(ctx, "pending-snapshot: settled", "snapshotId", doc.SnapshotID, "state", written.State)
	}
	return ok
}

// dropRefIfDeleted removes doc's reference again when its document is gone: a
// rollback or DeleteSnapshot may have removed both before write restored it.
func (r *PendingSnapshotReaper) dropRefIfDeleted(ctx context.Context, doc ebsmetadata.Snapshot) {
	_, err := r.svc.metadata.GetSnapshot(ctx, doc.OwnerID, doc.SnapshotID)
	if !objectstore.IsNoSuchKeyError(err) {
		return
	}
	if err := r.svc.removeSnapshotRef(ctx, doc.VolumeID, doc.SnapshotID); err != nil {
		slog.WarnContext(ctx, "pending-snapshot: reference of a deleted snapshot not removed",
			"snapshotId", doc.SnapshotID, "volumeId", doc.VolumeID, "err", err)
	}
}

// ReleaseNodeClaims releases every snapshot and image claim that names this
// node and no request of this process holds, whatever its document's state. A
// daemon runs it as it starts and before it serves requests: none survived.
func (s *SnapshotServiceImpl) ReleaseNodeClaims(ctx context.Context) (int, error) {
	docs, err := s.metadata.ListAllSnapshotsStrict(ctx)
	if err != nil {
		return 0, fmt.Errorf("list snapshot documents: %w", err)
	}
	amis, err := s.metadata.ListAMIsStrict(ctx)
	if err != nil {
		return 0, fmt.Errorf("list AMI documents: %w", err)
	}
	ids := make([]string, 0, len(docs)+len(amis))
	for _, doc := range docs {
		ids = append(ids, doc.SnapshotID)
	}
	for _, ami := range amis {
		ids = append(ids, ami.ImageID)
	}
	released := 0
	var errs []error
	for _, id := range ids {
		ok, err := s.claims.releaseOwn(ctx, id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ok {
			released++
		}
	}
	return released, errors.Join(errs...)
}
