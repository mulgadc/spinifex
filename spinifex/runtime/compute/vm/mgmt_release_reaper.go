package vm

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
)

// MgmtReleaseReaper retries management address releases that a proven stop
// recorded as pending but the address record never acknowledged.
type MgmtReleaseReaper struct{ m *Manager }

var _ Reaper = (*MgmtReleaseReaper)(nil)

// NewMgmtReleaseReaper returns the node-local retry for pending releases.
func (m *Manager) NewMgmtReleaseReaper() *MgmtReleaseReaper { return &MgmtReleaseReaper{m: m} }

func (r *MgmtReleaseReaper) Class() string      { return "mgmt-address-release" }
func (r *MgmtReleaseReaper) Scope() ReaperScope { return ScopeNodeLocal }

// Sweep releases each pending launch's address once the launch record still
// marks it pending and proves the launch stopped. Anything else is held.
func (r *MgmtReleaseReaper) Sweep(ctx context.Context) (int, error) {
	m := r.m
	if !m.mgmtDurable() {
		return 0, nil
	}
	entries, err := os.ReadDir(m.deps.MgmtLaunch.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	released := 0
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() || ctx.Err() != nil {
			continue
		}
		if r.retry(id) {
			released++
		}
	}
	return released, ctx.Err()
}

// retry re-reads the record under the instance's guard, so a launch that began
// since the scan has cleared the mark and keeps its address.
func (r *MgmtReleaseReaper) retry(id string) bool {
	m := r.m
	unlock := m.mgmtLocks.lock(id)
	defer unlock()
	rec, err := readMgmtLaunch(m.deps.MgmtLaunch.Dir, id)
	if err != nil {
		slog.Warn("Skipping unreadable management launch record", "instanceId", id, "err", err)
		return false
	}
	if !rec.ReleasePending {
		return false
	}
	hold := func(reason string, err error) {
		slog.Warn("Holding pending management address release", "instanceId", id, "ip", rec.MgmtIP,
			"generation", rec.Generation, "reason", reason, "err", err)
	}
	switch {
	case rec.Node != m.deps.NodeID:
		hold("launch record names another node", nil)
		return false
	case m.deps.MgmtLaunch.Addresses == nil:
		hold("management address record not wired", nil)
		return false
	}
	if err := m.mgmtProvenStopped(rec); err != nil {
		hold("launch not proven stopped", err)
		return false
	}
	return m.completeMgmtRelease(rec)
}
