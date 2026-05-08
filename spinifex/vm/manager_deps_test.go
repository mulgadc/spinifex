package vm

import (
	"maps"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/types"
)

// fakeStateStore is a minimal in-memory StateStore used to verify Deps wiring.
type fakeStateStore struct {
	saved      map[string]map[string]*VM
	stopped    map[string]*VM
	terminated map[string]*VM
}

func newFakeStateStore() *fakeStateStore {
	return &fakeStateStore{
		saved:      map[string]map[string]*VM{},
		stopped:    map[string]*VM{},
		terminated: map[string]*VM{},
	}
}

func (f *fakeStateStore) SaveRunningState(nodeID string, snap map[string]*VM) error {
	cp := make(map[string]*VM, len(snap))
	maps.Copy(cp, snap)
	f.saved[nodeID] = cp
	return nil
}

func (f *fakeStateStore) LoadRunningState(nodeID string) (map[string]*VM, error) {
	if v, ok := f.saved[nodeID]; ok {
		return v, nil
	}
	return map[string]*VM{}, nil
}

func (f *fakeStateStore) WriteStoppedInstance(id string, v *VM) error {
	f.stopped[id] = v
	return nil
}

func (f *fakeStateStore) LoadStoppedInstance(id string) (*VM, error) {
	if v, ok := f.stopped[id]; ok {
		return v, nil
	}
	return nil, nil
}

func (f *fakeStateStore) DeleteStoppedInstance(id string) error {
	delete(f.stopped, id)
	return nil
}

func (f *fakeStateStore) ListStoppedInstances() ([]*VM, error) {
	out := make([]*VM, 0, len(f.stopped))
	for _, v := range f.stopped {
		out = append(out, v)
	}
	return out, nil
}

func (f *fakeStateStore) WriteTerminatedInstance(id string, v *VM) error {
	f.terminated[id] = v
	return nil
}

func (f *fakeStateStore) ListTerminatedInstances() ([]*VM, error) {
	out := make([]*VM, 0, len(f.terminated))
	for _, v := range f.terminated {
		out = append(out, v)
	}
	return out, nil
}

var _ StateStore = (*fakeStateStore)(nil)

func TestNewManagerWithDeps_StoresDeps(t *testing.T) {
	store := newFakeStateStore()
	deps := Deps{
		NodeID:     "node-a",
		StateStore: store,
		ShutdownSignal: func() bool {
			return true
		},
	}
	m := NewManagerWithDeps(deps)

	if m.NodeID() != "node-a" {
		t.Fatalf("NodeID: got %q, want %q", m.NodeID(), "node-a")
	}
	if m.deps.StateStore == nil {
		t.Fatal("deps.StateStore: got nil after construction")
	}
	if !m.deps.ShutdownSignal() {
		t.Fatal("ShutdownSignal callback: not preserved")
	}

	// Sanity: the manager's map is independent of any future Deps state.
	if m.Count() != 0 {
		t.Fatalf("Count on fresh manager: got %d, want 0", m.Count())
	}
}

// fakeVolumeMounter records every call so lifecycle tests can assert ordering.
// The mutex covers the recording slices so StopAll's per-instance fan-out
// goroutines stay race-free.
type fakeVolumeMounter struct {
	mu                       sync.Mutex
	mounted, unmounted       []string
	mountedOne, unmountedOne []string
	mountErr                 error
	mountOneErr              error
	mountOneURI              string
	// onMount fires synchronously inside Mount before the configured
	// mountErr is returned. Used by lifecycle tests to simulate a
	// concurrent terminate flipping VM.Status while Mount is in flight.
	onMount func(*VM)
}

func (f *fakeVolumeMounter) Mount(v *VM) error {
	f.mu.Lock()
	f.mounted = append(f.mounted, v.ID)
	err := f.mountErr
	hook := f.onMount
	f.mu.Unlock()
	if hook != nil {
		hook(v)
	}
	return err
}

func (f *fakeVolumeMounter) Unmount(v *VM) error {
	f.mu.Lock()
	f.unmounted = append(f.unmounted, v.ID)
	f.mu.Unlock()
	return nil
}

func (f *fakeVolumeMounter) MountOne(req *types.EBSRequest) error {
	f.mu.Lock()
	f.mountedOne = append(f.mountedOne, req.Name)
	mountOneErr := f.mountOneErr
	uri := f.mountOneURI
	f.mu.Unlock()
	if mountOneErr != nil {
		return mountOneErr
	}
	if uri != "" {
		req.NBDURI = uri
	}
	return nil
}

func (f *fakeVolumeMounter) UnmountOne(req types.EBSRequest) {
	f.mu.Lock()
	f.unmountedOne = append(f.unmountedOne, req.Name)
	f.mu.Unlock()
}

var _ VolumeMounter = (*fakeVolumeMounter)(nil)
