//test:in-package — drives the teardown reaper's unexported QEMU gate through
//one Sweep over the shared manager and state-store doubles.

package vm

import (
	"context"
	"errors"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gateCallLog records the order of the UEFI removal and the volume delete.
type gateCallLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *gateCallLog) add(call string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, call)
}

func (l *gateCallLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

type orderedCleaner struct {
	*recordingInstanceCleaner

	log *gateCallLog
}

func (c orderedCleaner) DeleteVolumes(v *VM) error {
	c.log.add("DeleteVolumes")
	return c.recordingInstanceCleaner.DeleteVolumes(v)
}

type orderedUEFIStores struct {
	*fakeUEFIStores

	log *gateCallLog
}

func (u orderedUEFIStores) RemoveLocalUEFIStore(id string) error {
	u.log.add("RemoveLocalUEFIStore")
	return u.fakeUEFIStores.RemoveLocalUEFIStore(id)
}

// snapshotStore lists copies of the stored records, so a sweep that changes
// a listed record changes nothing stored until it writes the record back.
type snapshotStore struct {
	*fakeStateStore
}

func (s snapshotStore) ListTerminatedInstances() ([]*VM, error) {
	listed, err := s.fakeStateStore.ListTerminatedInstances()
	if err != nil {
		return nil, err
	}
	out := make([]*VM, len(listed))
	for i, v := range listed {
		out[i] = &VM{ID: v.ID, Status: v.Status, LastNode: v.LastNode,
			TerminatedAt: v.TerminatedAt, Teardown: maps.Clone(v.Teardown)}
	}
	return out, nil
}

// storedQEMUMark reads the qemu teardown mark from the record the fake store
// holds now, and reports whether the record is still there.
func storedQEMUMark(store *fakeStateStore, id string) (string, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	rec, ok := store.terminated[id]
	if !ok {
		return "", false
	}
	return rec.Teardown[TeardownQEMU], true
}

// The QEMU gate decides three outputs together: whether the local UEFI copy is
// removed, whether the QEMU teardown completes, and whether volumes are deleted.
// A change to the gate has to change this table on purpose.
func TestTeardownReaperQEMUGateDecisionTable(t *testing.T) {
	cases := []struct {
		name            string
		qemuAlive       bool
		lastNode        string
		qemuOutstanding bool
		uefiRemoveErr   error
		wantCalls       []string
		wantQEMU        TeardownState
	}{
		{name: "qemu alive on this node, qemu outstanding", qemuAlive: true, lastNode: "test-node", qemuOutstanding: true,
			wantQEMU: TeardownFailed},
		{name: "qemu alive on this node, qemu already done", qemuAlive: true, lastNode: "test-node",
			wantQEMU: TeardownDone},
		{name: "qemu gone on this node", lastNode: "test-node", qemuOutstanding: true,
			wantCalls: []string{"RemoveLocalUEFIStore", "DeleteVolumes"}, wantQEMU: TeardownDone},
		{name: "qemu gone on this node, uefi removal fails", lastNode: "test-node", qemuOutstanding: true,
			uefiRemoveErr: errors.New("unlink: device busy"),
			wantCalls:     []string{"RemoveLocalUEFIStore", "DeleteVolumes"}, wantQEMU: TeardownFailed},
		{name: "no recorded node, qemu outstanding", qemuOutstanding: true,
			wantQEMU: TeardownFailed},
		{name: "no recorded node, qemu done",
			wantCalls: []string{"DeleteVolumes"}, wantQEMU: TeardownDone},
		{name: "qemu done on this node", lastNode: "test-node",
			wantCalls: []string{"DeleteVolumes"}, wantQEMU: TeardownDone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// newStuckTerminateReaper gives each row its own runtime directory,
			// so a PID file never leaks from one row to the next.
			log := &gateCallLog{}
			cleaner := orderedCleaner{recordingInstanceCleaner: &recordingInstanceCleaner{}, log: log}
			stuck, store := newStuckTerminateReaper(t, cleaner)
			deps := stuck.m.deps
			deps.UEFIStores = orderedUEFIStores{fakeUEFIStores: &fakeUEFIStores{removeErr: tc.uefiRemoveErr}, log: log}
			deps.StateStore = snapshotStore{fakeStateStore: store}
			stuck.m.SetDeps(deps)

			const id = "i-gate"
			if tc.qemuAlive {
				liveQEMU(t, id)
			}
			qemu := TeardownDone
			if tc.qemuOutstanding {
				qemu = TeardownFailed
			}
			// A fresh TerminatedAt keeps a completed record inside the visibility
			// window, so the sweep merges its marks rather than purging it.
			require.NoError(t, store.WriteTerminatedInstance(id, &VM{
				ID: id, Status: StateTerminated, LastNode: tc.lastNode, TerminatedAt: time.Now(),
				Teardown: map[string]string{TeardownQEMU: string(qemu), TeardownVolumes: string(TeardownFailed)},
			}))

			_, err := stuck.m.NewTerminatedTeardownReaper().Sweep(context.Background())
			require.NoError(t, err)

			assert.Equal(t, tc.wantCalls, log.snapshot(), "gate side effects, in order")
			mark, ok := storedQEMUMark(store, id)
			require.True(t, ok, "the record must still be in the store")
			assert.Equal(t, string(tc.wantQEMU), mark, "qemu teardown state")
		})
	}
}
