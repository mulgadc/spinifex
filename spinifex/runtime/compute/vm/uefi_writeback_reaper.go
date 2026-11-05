package vm

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
)

// UEFIWriteBackReaper retries every write-back this node owes for an instance
// whose guest is not running here, and prunes the prefixes its write-backs
// left superseded objects under.
type UEFIWriteBackReaper struct {
	m *Manager
}

// NewUEFIWriteBackReaper returns the reaper, or nil on a node that cannot
// write a store back.
func (m *Manager) NewUEFIWriteBackReaper() *UEFIWriteBackReaper {
	if !m.uefiWriteBackWired() {
		return nil
	}
	return &UEFIWriteBackReaper{m: m}
}

func (r *UEFIWriteBackReaper) Class() string      { return "uefi-write-back" }
func (r *UEFIWriteBackReaper) Scope() ReaperScope { return ScopeNodeLocal }

// Sweep settles each owed store and then prunes, reporting how many stores it
// settled or dropped plus how many objects it deleted.
func (r *UEFIWriteBackReaper) Sweep(ctx context.Context) (int, error) {
	m := r.m
	dirs, err := filepath.Glob(filepath.Join(m.deps.UEFIVarStore.DataDir, uefiStoreDirName, "i-*"))
	if err != nil {
		return 0, err
	}
	reaped := 0
	for _, dir := range dirs {
		if m.settleOwedUEFIStore(ctx, filepath.Base(dir)) {
			reaped++
		}
	}
	return reaped + m.pruneUEFIStores(ctx), nil
}

// uefiGuestMayRun reports whether an instance in status may have a guest
// running here, or is being stopped by a path that settles its store itself.
func uefiGuestMayRun(status InstanceState) bool {
	switch status {
	case StateProvisioning, StatePending, StateRunning, StateStopping, StateShuttingDown:
		return true
	}
	return false
}

// settleOwedUEFIStore writes back id's owed store and unlocks its claim. A
// claim another node holds means that node took the instance over, so the
// owed changes can never land: the mark and the local copy go (ADR-0003:S7).
func (m *Manager) settleOwedUEFIStore(ctx context.Context, id string) bool {
	unlock := m.uefi.locks.lock(id)
	defer unlock()
	if m.uefi.get(id) != nil {
		return false
	}
	if v, ok := m.Get(id); ok && uefiGuestMayRun(m.Status(v)) {
		return false
	}
	dir, err := uefiInstanceDir(m.deps.UEFIVarStore.DataDir, id)
	if err != nil {
		return false
	}
	seed, err := readUEFISeed(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("vm/gc: cannot read a local UEFI variable store record; keeping it", "instanceId", id, "err", err)
		}
		return false
	}
	if !seed.WriteBackOwed {
		return false
	}
	w, _, claim, err := m.uefiWriterFromSeed(ctx, id)
	if err != nil {
		slog.Warn("vm/gc: cannot read the claim of an owed UEFI variable store; retrying next sweep", "instanceId", id, "err", err)
		return false
	}
	if claim == uefiClaimElsewhere {
		m.dropOwedUEFIStore(id, dir, "another node holds the claim")
		return true
	}
	switch m.settleUEFIStore(ctx, w, true, "reaper", "the owed write-back landed") {
	case uefiWriteBackLanded:
		return true
	case uefiWriteBackFenced:
		m.dropOwedUEFIStore(id, dir, "a later claim fenced the store record")
		return true
	case uefiWriteBackGone:
		removeUEFIDir(id, dir)
		return true
	}
	return false
}

// dropOwedUEFIStore gives up a write-back that can never land because another
// node owns the store now, counting the changes lost.
func (m *Manager) dropOwedUEFIStore(id, dir, why string) {
	recordUEFIWriteBackLost()
	slog.Warn("vm/gc: dropping an owed UEFI variable store write-back; its changes are lost",
		"instanceId", id, "why", why)
	removeUEFIDir(id, dir)
}

func removeUEFIDir(id, dir string) {
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("vm/gc: failed to remove a local UEFI variable store", "instanceId", id, "err", err)
	}
}

// pruneUEFIStores deletes the objects no pointer names under each queued
// prefix, keeping the record's pointer and anything younger than
// PruneMinAge. A prefix with young leftovers, or a failed prune, is queued again.
func (m *Manager) pruneUEFIStores(ctx context.Context) int {
	deps := m.deps.UEFIVarStore
	cutoff := m.uefiClock().Now().Add(-uefistore.PruneMinAge)
	deleted := 0
	for _, id := range m.uefi.drainPrune() {
		rec, _, err := deps.Records.Get(ctx, id)
		if errors.Is(err, kvstore.ErrNotFound) {
			continue
		}
		if err != nil {
			slog.Warn("vm/gc: cannot read a UEFI store record to prune; retrying next sweep", "instanceId", id, "err", err)
			m.uefi.queuePrune(id)
			continue
		}
		if rec.State != uefistore.StateLive || rec.Pointer == nil {
			continue
		}
		res, err := uefistore.PruneObjects(ctx, deps.Objects, deps.Bucket, id, rec.Pointer.Key, cutoff)
		deleted += res.Deleted
		recordUEFIPruned(res.Deleted)
		if err != nil {
			slog.Warn("vm/gc: UEFI store prune failed; retrying next sweep", "instanceId", id, "err", err)
		}
		if err != nil || res.Young > 0 {
			m.uefi.queuePrune(id)
		}
	}
	return deleted
}
