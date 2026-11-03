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

// uefiInstanceRecords is what the store reaper reads of the instance records:
// enough to tell a decided terminate from a live instance.
type uefiInstanceRecords interface {
	LoadInstanceRecord(instanceID string) (*vm.InstanceRecord, error)
	LoadTerminatedInstance(instanceID string) (*vm.VM, error)
	ListTerminatedInstances() ([]*vm.VM, error)
	UpdateTerminatedInstance(instanceID string, mutate func(*vm.VM)) (*vm.VM, error)
}

// uefiStoreReaper deletes the variable stores whose record is deleting: each
// object under the instance's prefix, then the record. Terminate, or an
// operator's forget, sets deleting and returns, so this decides nothing.
//
// It also completes a terminate whose mark failed, from the terminated
// record's pending or failed uefi entry. That record is the terminate itself,
// so moving the store record to deleting there is still not the reaper's call.
//
// Cluster-wide: the store outlives the node that ran the instance, and one
// sweeper keeps two deletes off one prefix.
type uefiStoreReaper struct {
	stores    *uefistore.Store
	objects   objectstore.ObjectStore
	bucket    string
	instances uefiInstanceRecords
}

var _ vm.Reaper = (*uefiStoreReaper)(nil)

// newUEFIStoreReaper builds the reaper, or nil when this node has no store
// bucket or object store to reap with.
func (d *Daemon) newUEFIStoreReaper() *uefiStoreReaper {
	if d.jsManager == nil || d.jsManager.UEFIStores() == nil || d.config == nil || d.config.Predastore.Bucket == "" {
		return nil
	}
	return &uefiStoreReaper{
		stores: d.jsManager.UEFIStores(),
		objects: objectstore.NewS3ObjectStoreFromConfig(netaddr.DialTarget(d.config.Predastore.Host),
			d.config.Predastore.Region, d.config.Predastore.AccessKey, d.config.Predastore.SecretKey),
		bucket:    d.config.Predastore.Bucket,
		instances: d.jsManager,
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

	ok, err := r.terminateDecided(id)
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
	if err := r.markTerminatedDone(id); err != nil {
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

// terminateDecided reports whether id's instance is terminated or terminating,
// or has no record at all, which is what a forget or an expired terminated
// record leaves.
func (r *uefiStoreReaper) terminateDecided(id string) (bool, error) {
	terminated, err := r.instances.LoadTerminatedInstance(id)
	if err != nil {
		return false, err
	}
	if terminated != nil {
		return true, nil
	}
	record, err := r.instances.LoadInstanceRecord(id)
	if err != nil {
		return false, err
	}
	if record == nil {
		return true, nil
	}
	switch record.Status.Status {
	case vm.StateShuttingDown, vm.StateTerminated:
		return true, nil
	}
	return false, nil
}

// markTerminatedDone sets the terminated record's uefi entry done, if the
// record is still there to carry one.
func (r *uefiStoreReaper) markTerminatedDone(id string) error {
	terminated, err := r.instances.LoadTerminatedInstance(id)
	if err != nil {
		return err
	}
	if terminated == nil || vm.TeardownState(terminated.Teardown[vm.TeardownUEFI]) == vm.TeardownDone {
		return nil
	}
	_, err = r.instances.UpdateTerminatedInstance(id, func(v *vm.VM) {
		if v.Teardown == nil {
			v.Teardown = make(map[string]string)
		}
		v.Teardown[vm.TeardownUEFI] = string(vm.TeardownDone)
	})
	if errors.Is(err, kvstore.ErrNotFound) {
		return nil
	}
	return err
}

// completeTerminatedMarks finishes the mark of each terminate that recorded a
// uefi entry but did not land its store record in deleting.
func (r *uefiStoreReaper) completeTerminatedMarks(ctx context.Context) {
	terminated, err := r.instances.ListTerminatedInstances()
	if err != nil {
		slog.WarnContext(ctx, "uefi-store: cannot list terminated instances", "err", err)
		return
	}
	for _, v := range terminated {
		state, ok := v.Teardown[vm.TeardownUEFI]
		if !ok || vm.TeardownState(state) == vm.TeardownDone || v.Status != vm.StateTerminated {
			continue
		}
		rec, _, err := r.stores.Get(ctx, v.ID)
		switch {
		case errors.Is(err, kvstore.ErrNotFound):
			// Nothing left to delete: the store is gone or never existed.
			if err := r.markTerminatedDone(v.ID); err != nil {
				slog.WarnContext(ctx, "uefi-store: failed to mark the terminated record", "instanceId", v.ID, "err", err)
			}
		case err != nil:
			slog.WarnContext(ctx, "uefi-store: unreadable store record", "instanceId", v.ID, "err", err)
		case rec.State == uefistore.StateLive:
			if _, err := r.stores.MarkDeleting(ctx, v.ID); err != nil {
				slog.WarnContext(ctx, "uefi-store: failed to record a terminated instance's store for deletion",
					"instanceId", v.ID, "err", err)
			}
		}
	}
}
