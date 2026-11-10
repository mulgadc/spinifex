package snapshot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
)

// PendingSnapshotReaper settles every pending snapshot document from the
// provider. CreateSnapshot returns once the provider answers, and a snapshot
// that settles later, or a request that crashed or failed ambiguously, leaves
// its document pending; this sweep is where that work converges (ADR-0003:S8).
//
// It deletes nothing (ADR-0003:S7). It only moves a pending document to
// completed or error, or records the provider's handle on it.
type PendingSnapshotReaper struct {
	svc *SnapshotServiceImpl
}

var _ vm.Reaper = (*PendingSnapshotReaper)(nil)

// NewPendingSnapshotReaper builds the reaper over this service's documents,
// provider and claims.
func (s *SnapshotServiceImpl) NewPendingSnapshotReaper() *PendingSnapshotReaper {
	return &PendingSnapshotReaper{svc: s}
}

func (r *PendingSnapshotReaper) Class() string { return "pending-snapshot" }

// Scope is cluster-wide: one sweep per cluster reads the provider for each
// pending document, and the claim, not the scope, decides who may repeat a call.
func (r *PendingSnapshotReaper) Scope() vm.ReaperScope { return vm.ScopeClusterWide }

// Sweep reads every snapshot document and settles each pending one it can,
// reporting how many it wrote. A document it cannot settle now is left for
// the next sweep.
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
		if doc.State != snapshotStatePending {
			continue
		}
		if r.settle(ctx, doc) {
			written++
		}
	}
	return written, nil
}

// settle follows the provider's state for doc, or repeats its CreateSnapshot
// when the provider holds no snapshot by that ID. It reports whether it wrote.
func (r *PendingSnapshotReaper) settle(ctx context.Context, doc ebsmetadata.Snapshot) bool {
	resp, err := r.svc.provider.ListSnapshots(ctx, ebsprovider.ListSnapshotsRequest{
		Versioned: ebsprovider.NewLatestVersioned(), SnapshotID: doc.SnapshotID,
	})
	if err != nil {
		slog.WarnContext(ctx, "pending-snapshot: provider read failed, retrying next sweep",
			"snapshotId", doc.SnapshotID, "accountId", doc.OwnerID, "err", err)
		return false
	}
	if resp != nil && len(resp.Snapshots) > 0 {
		return r.follow(ctx, doc, resp.Snapshots[0])
	}
	return r.repeat(ctx, doc)
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
	return r.write(ctx, doc, mutate)
}

// repeat asks the provider again for a snapshot it does not hold, under the
// snapshot's claim. Absence alone settles nothing, because a call the creating
// request sent may still be accepted; the repeat returns that snapshot or makes it.
func (r *PendingSnapshotReaper) repeat(ctx context.Context, doc ebsmetadata.Snapshot) bool {
	held, err := r.svc.claims.take(ctx, doc.SnapshotID)
	if err != nil {
		level := slog.LevelWarn
		if errors.Is(err, errClaimHeld) {
			level = slog.LevelInfo
		}
		slog.Log(ctx, level, "pending-snapshot: claim not taken, retrying next sweep",
			"snapshotId", doc.SnapshotID, "err", err)
		return false
	}
	defer held.release(ctx)

	// The claim may have been taken after the creating request rolled back or
	// settled the document, so it is read again before the provider is called.
	current, err := r.svc.metadata.GetSnapshot(ctx, doc.OwnerID, doc.SnapshotID)
	if err != nil || current.State != snapshotStatePending {
		return false
	}

	created, err := r.svc.provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{
		Versioned: ebsprovider.NewVersioned(), SnapshotID: current.SnapshotID, VolumeID: current.VolumeID,
	})
	switch {
	case err == nil && created != nil:
		return r.write(ctx, current, applyProviderAnswer(created))
	case refusalSettlesError(err):
		slog.WarnContext(ctx, "pending-snapshot: provider refused the repeat for good, marking the snapshot error",
			"snapshotId", current.SnapshotID, "volumeId", current.VolumeID, "err", err)
		return r.write(ctx, current, markSnapshotError)
	default:
		slog.WarnContext(ctx, "pending-snapshot: repeat not answered, retrying next sweep",
			"snapshotId", current.SnapshotID, "volumeId", current.VolumeID, "err", err)
		return false
	}
}

// write applies mutate to doc's current document while it is still pending.
func (r *PendingSnapshotReaper) write(ctx context.Context, doc ebsmetadata.Snapshot, mutate func(*ebsmetadata.Snapshot)) bool {
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

// ReleaseNodeClaims releases every claim on a pending snapshot that names this
// node and no request of this process holds. A daemon runs it as it starts and
// before it serves requests, since no request of its earlier process survived.
func (s *SnapshotServiceImpl) ReleaseNodeClaims(ctx context.Context) (int, error) {
	docs, err := s.metadata.ListAllSnapshotsStrict(ctx)
	if err != nil {
		return 0, fmt.Errorf("list snapshot documents: %w", err)
	}
	released := 0
	var errs []error
	for _, doc := range docs {
		if doc.State != snapshotStatePending {
			continue
		}
		ok, err := s.claims.releaseOwn(ctx, doc.SnapshotID)
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
