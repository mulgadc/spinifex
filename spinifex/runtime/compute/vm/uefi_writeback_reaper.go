package vm

import (
	"context"
	"errors"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
)

// uefiWriteBackReaper retries this node's owed variable store write-backs
// from the copy each stop kept, putting to the key its obligation recorded.
// It moves a pointer only while the instance is still this node's and stopped.
type uefiWriteBackReaper struct {
	m *Manager
}

var _ Reaper = (*uefiWriteBackReaper)(nil)

// NewUEFIWriteBackReaper builds the reaper over this Manager's variable store
// dependencies.
func (m *Manager) NewUEFIWriteBackReaper() Reaper {
	return &uefiWriteBackReaper{m: m}
}

func (r *uefiWriteBackReaper) Class() string { return "uefi-write-back" }

func (r *uefiWriteBackReaper) Scope() ReaperScope { return ScopeNodeLocal }

// Sweep settles each obligation this node owes, reporting how many it
// settled. One that cannot be decided yet stays for the next sweep.
func (r *uefiWriteBackReaper) Sweep(ctx context.Context) (int, error) {
	m := r.m
	deps := m.deps.UEFIVarStore
	if !deps.writeBackWired() || m.deps.NodeID == "" {
		return 0, nil
	}
	ids, err := deps.Records.WriteBacksOwedBy(ctx, m.deps.NodeID)
	if err != nil {
		return 0, err
	}
	settled := 0
	var errs []error
	for _, id := range ids {
		done, err := m.retryUEFIWriteBack(ctx, id)
		if err != nil {
			slog.WarnContext(ctx, "UEFI write-back retry did not settle; retrying next sweep", "instanceId", id, "err", err)
			errs = append(errs, err)
		}
		if done {
			settled++
		}
	}
	return settled, errors.Join(errs...)
}

// retryUEFIWriteBack settles id's obligation if it is still this node's: it is
// cleared when the record already points at its key, written back while the
// record is stopped here at the pointer it seeded from, and dropped otherwise.
func (m *Manager) retryUEFIWriteBack(ctx context.Context, id string) (bool, error) {
	unlock := m.uefiLocks.lock(id)
	defer unlock()

	deps := m.deps.UEFIVarStore
	owed, err := deps.Records.LiveWriteBack(ctx, id)
	if errors.Is(err, uefistore.ErrNotLive) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if owed == nil || owed.Node != m.deps.NodeID {
		return false, nil
	}
	dir, err := uefiInstanceDir(deps.DataDir, id)
	if err != nil {
		return false, err
	}

	// An instance in this node's running set has its record written from
	// memory, so its pointer is moved there rather than in KV.
	if local, ok := m.Get(id); ok {
		return m.retryLocalUEFIWriteBack(ctx, local, dir, *owed)
	}

	owner, found, err := deps.Instances.LoadUEFIVarStoreOwner(id)
	if err != nil {
		return false, err
	}
	var pointer string
	if found && owner.Pointer != nil {
		pointer = owner.Pointer.Key
	}
	switch {
	case pointer == owed.Key:
		// An earlier record write landed without its reply.
		return m.landedUEFIWriteBack(ctx, id, dir, *owed)
	case !found || owner.Status != StateStopped || owner.LastNode != m.deps.NodeID || pointer != owed.SeededFrom:
		return m.dropUEFIWriteBack(ctx, id, *owed, pointer, "the record is no longer stopped here at the pointer this node seeded from")
	}
	if seed, err := readUEFISeed(dir); err != nil || seed != owed.SeededFrom {
		return m.dropUEFIWriteBack(ctx, id, *owed, pointer, "this node's copy is no longer the one seeded from the pointer")
	}

	written, err := m.putUEFIVars(ctx, id, dir, owed.Key, owner.Pointer.Firmware.Arch)
	if err != nil {
		return false, err
	}
	if settled, err := m.confirmUEFIWriteBackOwed(ctx, id, *owed); !settled {
		return false, err
	}
	if err := deps.Instances.CompareAndSetUEFIVarStore(id, written, owner.Revision); err != nil {
		return false, err
	}
	m.finishUEFIWriteBack(ctx, id, dir, owed.Key, owed.SeededFrom, owed.Key)
	recordUEFIWriteBackOutcome("retried")
	slog.InfoContext(ctx, "Wrote an owed UEFI variable store back", "instanceId", id, "key", owed.Key, "since", owed.Since)
	return true, nil
}

// retryLocalUEFIWriteBack is retryUEFIWriteBack for an instance this node's
// running set holds, as a drain-stopped one is until the node restarts.
func (m *Manager) retryLocalUEFIWriteBack(ctx context.Context, local *VM, dir string, owed uefistore.UEFIWriteBack) (bool, error) {
	id := local.ID
	var status InstanceState
	var previous *UEFIVarStoreRef
	m.Inspect(local, func(v *VM) {
		status = v.Status
		if v.UEFIVarStore != nil {
			cp := *v.UEFIVarStore
			previous = &cp
		}
	})
	var pointer string
	if previous != nil {
		pointer = previous.Key
	}
	switch {
	case pointer == owed.Key:
		return m.landedUEFIWriteBack(ctx, id, dir, owed)
	case status != StateStopped || pointer != owed.SeededFrom:
		return m.dropUEFIWriteBack(ctx, id, owed, pointer, "this node's instance has moved on from the pointer it seeded from")
	}
	if seed, err := readUEFISeed(dir); err != nil || seed != owed.SeededFrom {
		return m.dropUEFIWriteBack(ctx, id, owed, pointer, "this node's copy is no longer the one seeded from the pointer")
	}

	written, err := m.putUEFIVars(ctx, id, dir, owed.Key, previous.Firmware.Arch)
	if err != nil {
		return false, err
	}
	if settled, err := m.confirmUEFIWriteBackOwed(ctx, id, owed); !settled {
		return false, err
	}
	m.Inspect(local, func(v *VM) { v.UEFIVarStore = written })
	if err := m.writeRunningState(); err != nil {
		m.Inspect(local, func(v *VM) {
			if v.UEFIVarStore != nil && v.UEFIVarStore.Key == owed.Key {
				v.UEFIVarStore = previous
			}
		})
		return false, err
	}
	m.finishUEFIWriteBack(ctx, id, dir, owed.Key, owed.SeededFrom, owed.Key)
	recordUEFIWriteBackOutcome("retried")
	slog.InfoContext(ctx, "Wrote an owed UEFI variable store back", "instanceId", id, "key", owed.Key, "since", owed.Since)
	return true, nil
}

// confirmUEFIWriteBackOwed re-reads the obligation after a retry's put. A store
// gone or deleting, or an obligation cleared meanwhile, leaves the put
// pointed at by nothing, so it is deleted and the retry stops.
func (m *Manager) confirmUEFIWriteBackOwed(ctx context.Context, id string, owed uefistore.UEFIWriteBack) (bool, error) {
	cur, err := m.deps.UEFIVarStore.Records.LiveWriteBack(ctx, id)
	switch {
	case errors.Is(err, uefistore.ErrNotLive):
		m.deleteUEFIObject(ctx, id, owed.Key)
		recordUEFIWriteBackOutcome("dropped")
		return false, nil
	case err != nil:
		return false, err
	case cur == nil || cur.Key != owed.Key || cur.Node != owed.Node:
		m.deleteUEFIObject(ctx, id, owed.Key)
		recordUEFIWriteBackOutcome("dropped")
		return false, nil
	}
	return true, nil
}

// landedUEFIWriteBack clears an obligation whose key the record already
// points at, and lets a start here reuse the copy that key holds.
func (m *Manager) landedUEFIWriteBack(ctx context.Context, id, dir string, owed uefistore.UEFIWriteBack) (bool, error) {
	if seed, err := readUEFISeed(dir); err != nil || seed != owed.SeededFrom {
		// The copy here is not the one that was put, so seeded.json keeps
		// whatever it names and only the obligation is settled.
		if err := m.deps.UEFIVarStore.Records.ClearWriteBack(ctx, id, owed.Key); err != nil && !errors.Is(err, uefistore.ErrNotLive) {
			return false, err
		}
		recordUEFIWriteBackOutcome("landed")
		return true, nil
	}
	m.finishUEFIWriteBack(ctx, id, dir, owed.Key, owed.SeededFrom, owed.Key)
	recordUEFIWriteBackOutcome("landed")
	return true, nil
}

// dropUEFIWriteBack clears a superseded obligation, then deletes the object
// at its key, which no record points at. pointer is the record's current key.
func (m *Manager) dropUEFIWriteBack(ctx context.Context, id string, owed uefistore.UEFIWriteBack, pointer, why string) (bool, error) {
	err := m.deps.UEFIVarStore.Records.ClearWriteBack(ctx, id, owed.Key)
	if errors.Is(err, uefistore.ErrNotLive) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if pointer != owed.Key {
		m.deleteUEFIObject(ctx, id, owed.Key)
	}
	recordUEFIWriteBackOutcome("superseded")
	slog.InfoContext(ctx, "Dropped a superseded UEFI write-back obligation: "+why, "instanceId", id,
		"key", owed.Key, "seededFrom", owed.SeededFrom, "pointer", pointer)
	return true, nil
}
