package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/spinifex/foundation/netaddr"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
	"github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
)

// uefiStoreKVTimeout bounds the one record read and write a terminate makes.
const uefiStoreKVTimeout = 10 * time.Second

// uefiLocalDirName is the directory under the daemon's data directory that
// holds each instance's local copy of its variable store.
const uefiLocalDirName = "uefi"

// uefiLocalDir is the directory holding instanceID's local variable store.
func uefiLocalDir(dataDir, instanceID string) string {
	if dataDir == "" {
		dataDir = DefaultDataDir
	}
	return filepath.Join(dataDir, uefiLocalDirName, instanceID)
}

// uefiStoreTerminator satisfies vm.UEFIStoreTerminator over the store-record
// bucket and this node's data directory.
type uefiStoreTerminator struct {
	stores  *uefistore.Store
	dataDir string
}

var _ vm.UEFIStoreTerminator = (*uefiStoreTerminator)(nil)

// newUEFIStoreTerminator returns nil, not a typed nil, when the bucket is not
// open, so the manager sees no terminator rather than one that panics.
func (d *Daemon) newUEFIStoreTerminator() vm.UEFIStoreTerminator {
	if d.jsManager == nil || d.jsManager.UEFIStores() == nil {
		return nil
	}
	return &uefiStoreTerminator{stores: d.jsManager.UEFIStores(), dataDir: d.config.DataDir}
}

func (t *uefiStoreTerminator) MarkUEFIStoreDeleting(instanceID string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), uefiStoreKVTimeout)
	defer cancel()
	return t.stores.MarkDeleting(ctx, instanceID)
}

// RemoveLocalUEFIStore refuses anything but a bare instance ID, so a malformed
// ID cannot point the removal outside the instance's own directory.
func (t *uefiStoreTerminator) RemoveLocalUEFIStore(instanceID string) error {
	if !strings.HasPrefix(instanceID, "i-") || strings.ContainsAny(instanceID, `/\`) || strings.Contains(instanceID, "..") {
		return fmt.Errorf("refusing to remove a local UEFI store for %q", instanceID)
	}
	return os.RemoveAll(uefiLocalDir(t.dataDir, instanceID))
}

// uefiInstanceRecords is what the store reaper asks of the instance domain:
// whether a terminate is decided, and the terminated records' uefi entries.
type uefiInstanceRecords interface {
	TerminateDecided(instanceID string) (bool, error)
	OpenTeardowns(dep string) ([]string, error)
	QEMUTeardownSettled(instanceID string) (bool, error)
	MarkTeardownDone(instanceID, dep string) error
}

var _ uefiInstanceRecords = (*JetStreamManager)(nil)

// uefiStoreReaper deletes the variable stores whose record is deleting: each
// object under the instance's prefix, then the store's claim, then the record.
// A decided terminate sets deleting and returns, so this decides nothing.
//
// It also completes a terminate whose mark failed, from the terminated
// record's pending or failed uefi entry. That record is the terminate itself,
// so moving the store record to deleting there is still not the reaper's call.
// It waits while the record's QEMU teardown is open or the record is gone: the
// store is deleted only once QEMU is proven gone.
//
// Cluster-wide: the store outlives the node that ran the instance, and one
// sweeper keeps two deletes off one prefix.
type uefiStoreReaper struct {
	stores    *uefistore.Store
	objects   objectstore.ObjectStore
	bucket    string
	instances uefiInstanceRecords
	claims    *kvsync.ClaimGroup
}

var _ vm.Reaper = (*uefiStoreReaper)(nil)

// newUEFIStoreReaper builds the reaper, or nil when this node has no store
// bucket, claim group or object store to reap with.
func (d *Daemon) newUEFIStoreReaper() *uefiStoreReaper {
	if d.jsManager == nil || d.jsManager.UEFIStores() == nil || d.uefiClaims() == nil || d.config == nil || d.config.Predastore.Bucket == "" {
		return nil
	}
	return &uefiStoreReaper{
		stores: d.jsManager.UEFIStores(),
		objects: objectstore.NewS3ObjectStoreFromConfig(netaddr.DialTarget(d.config.Predastore.Host),
			d.config.Predastore.Region, d.config.Predastore.AccessKey, d.config.Predastore.SecretKey),
		bucket:    d.config.Predastore.Bucket,
		instances: d.jsManager,
		claims:    d.uefiClaims(),
	}
}

func (r *uefiStoreReaper) Class() string         { return "uefi-store" }
func (r *uefiStoreReaper) Scope() vm.ReaperScope { return vm.ScopeClusterWide }

// Sweep deletes every deleting store it can and reports how many records it
// retired. A store whose delete fails keeps its record, which has no TTL, and
// is retried next sweep however long predastore is away.
func (r *uefiStoreReaper) Sweep(ctx context.Context) (int, error) {
	r.completeTerminatedMarks(ctx)

	ids, err := r.stores.InstanceIDs(ctx)
	if err != nil {
		return 0, fmt.Errorf("list UEFI store records: %w", err)
	}
	reaped := 0
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return reaped, err
		}
		if r.reap(ctx, id) {
			reaped++
		}
	}
	return reaped, nil
}

// reap deletes one store if its record is deleting, and reports whether the
// record was retired.
func (r *uefiStoreReaper) reap(ctx context.Context, id string) bool {
	rec, rev, err := r.stores.Get(ctx, id)
	if err != nil {
		if !errors.Is(err, kvstore.ErrNotFound) {
			slog.WarnContext(ctx, "uefi-store: unreadable store record, leaving it", "instanceId", id, "err", err)
		}
		return false
	}
	if rec.State != uefistore.StateDeleting {
		return false
	}

	ok, err := r.instances.TerminateDecided(id)
	if err != nil {
		slog.WarnContext(ctx, "uefi-store: cannot read the instance record, retrying next sweep", "instanceId", id, "err", err)
		return false
	}
	if !ok {
		// Deleting is only ever set by a decided terminate or a forget, so a
		// live instance here is a defect; its store is kept for an operator.
		slog.ErrorContext(ctx, "uefi-store: refusing to delete the store of an instance that is not terminating",
			"instanceId", id, "accountId", rec.AccountID)
		return false
	}

	start := time.Now()
	deleted, err := uefistore.DeleteObjects(ctx, r.objects, r.bucket, id)
	if err != nil {
		slog.WarnContext(ctx, "uefi-store: delete incomplete, retrying next sweep",
			"instanceId", id, "deleted", deleted, "err", err)
		return false
	}
	if err := r.removeClaim(ctx, id); err != nil {
		slog.WarnContext(ctx, "uefi-store: failed to remove the store's claim, retrying next sweep", "instanceId", id, "err", err)
		return false
	}
	if err := r.instances.MarkTeardownDone(id, vm.TeardownUEFI); err != nil {
		slog.WarnContext(ctx, "uefi-store: failed to mark the terminated record, retrying next sweep", "instanceId", id, "err", err)
		return false
	}
	// Removed last: while it exists the obligation is readable, so a sweep
	// interrupted above resumes rather than leaving objects nobody owns.
	if err := r.stores.CompareAndDelete(ctx, id, rev); err != nil {
		slog.WarnContext(ctx, "uefi-store: failed to remove the store record, retrying next sweep", "instanceId", id, "err", err)
		return false
	}
	slog.InfoContext(ctx, "uefi-store: store deleted", "instanceId", id, "accountId", rec.AccountID,
		"objects", deleted, "elapsed_ms", otelsetup.Millis(time.Since(start)))
	return true
}

// removeClaimAttempts bounds removeClaim's retries against a holder that moves
// the key on between the read and the takeover.
const removeClaimAttempts = 3

// removeClaim deletes instanceID's store claim by taking it over from whatever
// holder it names and unlocking it. The store is deleting, so no stamp or
// pointer move can follow, and the holder needs no proof of being dead.
func (r *uefiStoreReaper) removeClaim(ctx context.Context, id string) error {
	claim := r.claims.Get(id)
	for range removeClaimAttempts {
		cur, held, err := claim.Holder(ctx)
		if err != nil {
			return err
		}
		if !held {
			return nil
		}
		tok, err := claim.TakeOver(ctx, cur)
		if _, moved := errors.AsType[*kvsync.HeldError](err); moved {
			continue
		}
		if err != nil {
			return err
		}
		err = claim.Unlock(ctx, tok)
		if errors.Is(err, kvsync.ErrLost) {
			continue
		}
		if err != nil {
			return err
		}
		slog.InfoContext(ctx, "uefi-store: claim removed", "instanceId", id, "holder", cur.Holder, "generation", cur.Generation)
		return nil
	}
	return fmt.Errorf("uefi-store: claim on %s kept moving after %d attempts", id, removeClaimAttempts)
}

// completeTerminatedMarks finishes the mark of each terminate that recorded a
// uefi entry but did not land its store record in deleting.
func (r *uefiStoreReaper) completeTerminatedMarks(ctx context.Context) {
	ids, err := r.instances.OpenTeardowns(vm.TeardownUEFI)
	if err != nil {
		slog.WarnContext(ctx, "uefi-store: cannot list terminated instances", "err", err)
		return
	}
	for _, id := range ids {
		if settled, err := r.instances.QEMUTeardownSettled(id); err != nil || !settled {
			slog.DebugContext(ctx, "uefi-store: QEMU not proven gone for a terminated instance, leaving its store",
				"instanceId", id, "err", err)
			continue
		}
		rec, _, err := r.stores.Get(ctx, id)
		switch {
		case errors.Is(err, kvstore.ErrNotFound):
			// Nothing left to delete: the store is gone or never existed.
			if err := r.instances.MarkTeardownDone(id, vm.TeardownUEFI); err != nil {
				slog.WarnContext(ctx, "uefi-store: failed to mark the terminated record", "instanceId", id, "err", err)
			}
		case err != nil:
			slog.WarnContext(ctx, "uefi-store: unreadable store record", "instanceId", id, "err", err)
		case rec.State == uefistore.StateLive:
			if ok, err := r.instances.TerminateDecided(id); err != nil || !ok {
				slog.WarnContext(ctx, "uefi-store: terminated instance still has a live record, leaving its store live",
					"instanceId", id, "err", err)
				continue
			}
			if _, err := r.stores.MarkDeleting(ctx, id); err != nil {
				slog.WarnContext(ctx, "uefi-store: failed to record a terminated instance's store for deletion",
					"instanceId", id, "err", err)
			}
		}
	}
}
