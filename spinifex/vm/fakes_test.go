package vm

import "sync"

// fakeResourceController records Allocate/Deallocate calls so lifecycle
// and crash-recovery tests can assert deallocation happened (or didn't)
// without standing up the daemon's real ResourceManager.
type fakeResourceController struct {
	mu             sync.Mutex
	allocated      map[string]int
	deallocated    []string
	allocateErr    error
	canAllocateRet int
}

func newFakeResourceController() *fakeResourceController {
	return &fakeResourceController{allocated: map[string]int{}, canAllocateRet: 100}
}

func (f *fakeResourceController) Allocate(typeName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.allocateErr != nil {
		return f.allocateErr
	}
	f.allocated[typeName]++
	return nil
}

func (f *fakeResourceController) Deallocate(typeName string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deallocated = append(f.deallocated, typeName)
	if f.allocated[typeName] > 0 {
		f.allocated[typeName]--
	}
}

func (f *fakeResourceController) CanAllocate(typeName string, count int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.canAllocateRet < count {
		return f.canAllocateRet
	}
	return count
}

func (f *fakeResourceController) deallocateCount(typeName string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, t := range f.deallocated {
		if t == typeName {
			n++
		}
	}
	return n
}

func (f *fakeResourceController) allocateCount(typeName string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.allocated[typeName]
}

var _ ResourceController = (*fakeResourceController)(nil)

// fakeInstanceTypeResolver is a static map satisfying InstanceTypeResolver.
type fakeInstanceTypeResolver map[string]InstanceTypeSpec

func (f fakeInstanceTypeResolver) Resolve(name string) (InstanceTypeSpec, bool) {
	spec, ok := f[name]
	return spec, ok
}

var _ InstanceTypeResolver = fakeInstanceTypeResolver(nil)

// recordedTransition is one entry in recordedTransitions.calls.
type recordedTransition struct {
	ID     string
	Target InstanceState
}

// recordedTransitions is a TransitionState seam that records every call so
// crash and shutdown tests can assert transition order. Writes to v.Status
// go through Manager.Inspect to match production's lock discipline; without
// it the goroutine MarkFailed spawns races with concurrent m.Status reads.
type recordedTransitions struct {
	mu    sync.Mutex
	m     *Manager
	calls []recordedTransition
	err   error
	waits []*pendingWait
}

// pendingWait is a single (id, target) the test wants notified on. apply
// closes done after v.Status has been updated, so receivers see the new
// status without polling.
type pendingWait struct {
	id     string
	target InstanceState
	done   chan struct{}
}

// bind associates the recorder with the manager whose lock guards v.Status
// reads. Must be called once before SetDeps installs the apply method.
func (r *recordedTransitions) bind(m *Manager) *recordedTransitions {
	r.mu.Lock()
	r.m = m
	r.mu.Unlock()
	return r
}

func (r *recordedTransitions) apply(v *VM, target InstanceState) error {
	r.mu.Lock()
	if r.err != nil {
		err := r.err
		r.mu.Unlock()
		return err
	}
	m := r.m
	r.mu.Unlock()

	// Publish Status before recording the call so any waitFor caller that
	// observes the recorded transition is guaranteed to see the new Status.
	if m != nil {
		m.Inspect(v, func(vv *VM) { vv.Status = target })
	} else {
		v.Status = target
	}

	r.mu.Lock()
	r.calls = append(r.calls, recordedTransition{ID: v.ID, Target: target})
	var matched []*pendingWait
	remaining := r.waits[:0]
	for _, pw := range r.waits {
		if pw.id == v.ID && pw.target == target {
			matched = append(matched, pw)
		} else {
			remaining = append(remaining, pw)
		}
	}
	r.waits = remaining
	r.mu.Unlock()

	for _, pw := range matched {
		close(pw.done)
	}
	return nil
}

// waitFor returns a channel closed once apply has recorded a (id, target)
// transition AND the corresponding v.Status flip has been published.
// Replaces require.Eventually polling on m.Status. If the transition has
// already happened the channel is returned already-closed.
func (r *recordedTransitions) waitFor(id string, target InstanceState) <-chan struct{} {
	done := make(chan struct{})
	r.mu.Lock()
	for _, c := range r.calls {
		if c.ID == id && c.Target == target {
			close(done)
			r.mu.Unlock()
			return done
		}
	}
	r.waits = append(r.waits, &pendingWait{id: id, target: target, done: done})
	r.mu.Unlock()
	return done
}

func (r *recordedTransitions) snapshot() []recordedTransition {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedTransition, len(r.calls))
	copy(out, r.calls)
	return out
}

func (r *recordedTransitions) targets(id string) []InstanceState {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []InstanceState
	for _, c := range r.calls {
		if c.ID == id {
			out = append(out, c.Target)
		}
	}
	return out
}

// recordingInstanceCleaner counts every InstanceCleaner method invocation so
// tests can assert e.g. "Stop must not call DeleteVolumes".
type recordingInstanceCleaner struct {
	mu                  sync.Mutex
	deleteVolumes       []string
	cleanupMgmt         []string
	releasePublicIP     []string
	detachAndDeleteENI  []string
	removeFromPlacement []string
	releaseGPU          []string
}

func (c *recordingInstanceCleaner) DeleteVolumes(v *VM) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleteVolumes = append(c.deleteVolumes, v.ID)
}

func (c *recordingInstanceCleaner) CleanupMgmtNetwork(v *VM) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cleanupMgmt = append(c.cleanupMgmt, v.ID)
}

func (c *recordingInstanceCleaner) ReleasePublicIP(v *VM) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releasePublicIP = append(c.releasePublicIP, v.ID)
}

func (c *recordingInstanceCleaner) DetachAndDeleteENI(v *VM) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.detachAndDeleteENI = append(c.detachAndDeleteENI, v.ID)
}

func (c *recordingInstanceCleaner) RemoveFromPlacementGroup(v *VM) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeFromPlacement = append(c.removeFromPlacement, v.ID)
}

func (c *recordingInstanceCleaner) ReleaseGPU(v *VM) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releaseGPU = append(c.releaseGPU, v.ID)
}

func (c *recordingInstanceCleaner) deleteVolumesCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.deleteVolumes)
}

var _ InstanceCleaner = (*recordingInstanceCleaner)(nil)
