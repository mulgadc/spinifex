package vm

import (
	"maps"
	"sync"
	"time"
)

// ManagerHooks are callbacks the manager fires synchronously on running/terminated
// transitions. Hook fields may be nil (no-ops). OnInstanceUp returns an error so
// reconnect callers can roll back QMP on subscribe failure; launch callers may ignore it.
type ManagerHooks struct {
	OnInstanceUp   func(*VM) error
	OnInstanceDown func(id string)
	// OnInstanceRecovering fires from Restore before each relaunch so the daemon
	// can early-subscribe ec2.cmd.<id> and handle concurrent terminates in flight.
	OnInstanceRecovering func(*VM)
	// BeforeInstanceRelaunch fires from Restore immediately before m.Run. Daemons
	// use this to refresh ephemeral on-host state that did not survive a reboot.
	// A non-nil error aborts the relaunch and marks the instance recovery_failed.
	BeforeInstanceRelaunch func(*VM) error
}

// Deps bundles collaborators the manager uses to drive lifecycle.
// Fields may be nil; call sites guard against nil so partial wiring is safe.
type Deps struct {
	NodeID string

	StateStore         StateStore
	VolumeMounter      VolumeMounter
	NetworkPlumber     NetworkPlumber
	InstanceTypes      InstanceTypeResolver
	Resources          ResourceController
	VolumeStateUpdater VolumeStateUpdater
	InstanceCleaner    InstanceCleaner
	Hooks              ManagerHooks

	// ShutdownSignal returns true once the daemon has begun coordinated
	// shutdown. Crash handlers and restart logic short-circuit on true.
	ShutdownSignal func() bool

	// CrashHandler is invoked from the QEMU exit goroutine when the process
	// terminates unexpectedly after startup was confirmed.
	CrashHandler func(*VM, error)

	// TransitionState applies a state transition to the supplied VM and
	// persists the resulting running-state snapshot.
	TransitionState func(*VM, InstanceState) error

	// DevNetworking enables the user-mode dev NIC (SSH hostfwd) on top of
	// the VPC tap NIC. Mirrors Daemon.config.Daemon.DevNetworking.
	DevNetworking bool
	// BindHost is the daemon's listen IP, used when wiring user-mode
	// hostfwd rules. Empty / "0.0.0.0" falls back to 127.0.0.1.
	BindHost string

	// DetachDelay is the pause between QMP device_del and blockdev-del
	// during DetachVolume, and the polling interval for blockdev-del retry.
	// Zero is acceptable in tests; production uses 1s.
	DetachDelay time.Duration

	// ConsumeCleanShutdownMarker reports whether the previous run wrote a clean
	// shutdown marker, consuming it. Nil is treated as "no marker" (cautious recovery).
	ConsumeCleanShutdownMarker func() bool
}

// Manager owns the in-memory map of running VMs on this node and every
// lifecycle transition that mutates that map.
type Manager struct {
	mu          sync.Mutex
	vms         map[string]*VM
	deps        Deps
	goroutineWg sync.WaitGroup
}

// NewManager returns a Manager with no collaborators wired. Tests that only
// exercise the in-memory map use this; production code uses NewManagerWithDeps.
func NewManager() *Manager {
	return &Manager{vms: make(map[string]*VM)}
}

// NewManagerWithDeps returns a Manager wired with collaborators.
func NewManagerWithDeps(deps Deps) *Manager {
	return &Manager{vms: make(map[string]*VM), deps: deps}
}

// SetDeps replaces the manager's dependencies. Called once collaborators are
// available after early construction. Must not be called concurrently.
func (m *Manager) SetDeps(deps Deps) { m.deps = deps }

// NodeID returns the node identifier the manager was constructed with.
func (m *Manager) NodeID() string { return m.deps.NodeID }

// Get returns the VM for id (and true) or (nil, false).
func (m *Manager) Get(id string) (*VM, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vms[id]
	return v, ok
}

// Insert unconditionally stores v under v.ID, overwriting any existing entry.
func (m *Manager) Insert(v *VM) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vms[v.ID] = v
}

// InsertIfAbsent stores v under v.ID only if no entry currently exists.
// Returns true if the insert happened, false if the slot was already occupied.
func (m *Manager) InsertIfAbsent(v *VM) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.vms[v.ID]; exists {
		return false
	}
	m.vms[v.ID] = v
	return true
}

// Delete removes the entry for id. No-op if absent.
func (m *Manager) Delete(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.vms, id)
}

// DeleteIf deletes the entry for id only if the stored pointer matches want.
// Returns true if the delete happened; false if the slot was reclaimed concurrently.
func (m *Manager) DeleteIf(id string, want *VM) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, exists := m.vms[id]
	if !exists || current != want {
		return false
	}
	delete(m.vms, id)
	return true
}

// Count returns the current number of VMs.
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.vms)
}

// ForEach calls fn for each VM under the manager lock. fn must not re-enter
// locked Manager methods (deadlock). For lock-free iteration use Snapshot.
func (m *Manager) ForEach(fn func(*VM)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.vms {
		fn(v)
	}
}

// Snapshot returns the current VMs as a slice. The returned pointers are shared
// with the manager; callers must not mutate map-related fields.
func (m *Manager) Snapshot() []*VM {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*VM, 0, len(m.vms))
	for _, v := range m.vms {
		out = append(out, v)
	}
	return out
}

// SnapshotMap returns a copy of the id→VM map for serialization outside the lock.
func (m *Manager) SnapshotMap() map[string]*VM {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]*VM, len(m.vms))
	maps.Copy(out, m.vms)
	return out
}

// Filter returns the VMs for which pred returns true. pred runs under lock.
func (m *Manager) Filter(pred func(*VM) bool) []*VM {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*VM
	for _, v := range m.vms {
		if pred(v) {
			out = append(out, v)
		}
	}
	return out
}

// UpdateState looks up id and, if found, runs fn(v) under lock.
// Returns true if the VM was found and fn was invoked.
func (m *Manager) UpdateState(id string, fn func(*VM)) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vms[id]
	if !ok {
		return false
	}
	fn(v)
	return true
}

// UpdateAndPersist looks up id and runs fn(v) under lock. fn returns true to
// trigger a state-store persist after the lock is released; false skips it.
// Returns (false, nil) when the VM is absent. On persist failure the in-memory
// mutation already took effect — memory stays ahead of disk until next persist.
func (m *Manager) UpdateAndPersist(id string, fn func(*VM) bool) (bool, error) {
	var changed bool
	found := m.UpdateState(id, func(v *VM) {
		changed = fn(v)
	})
	if !found || !changed {
		return found, nil
	}
	return true, m.writeRunningState()
}

// Status returns v.Status under the manager lock. No membership check —
// callers may pass a pointer to an instance no longer in the map.
func (m *Manager) Status(v *VM) InstanceState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return v.Status
}

// Inspect runs fn(v) under the manager lock for an already-resolved VM pointer.
// Prefer UpdateState for membership-checked mutation; Inspect is for closures
// that may run on a possibly-orphaned VM (e.g. MarkFailed).
func (m *Manager) Inspect(v *VM, fn func(*VM)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(v)
}

// View runs fn with the live id→VM map under the manager lock. fn must not
// mutate the map or retain references after returning.
func (m *Manager) View(fn func(map[string]*VM)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(m.vms)
}

// Replace bulk-resets the manager to the given VMs (copied). Used by Restore.
func (m *Manager) Replace(vms map[string]*VM) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vms = make(map[string]*VM, len(vms))
	maps.Copy(m.vms, vms)
}

// WaitForBackgroundWork blocks until all goroutines started by MarkFailed and
// MarkRecoveryFailed complete. Used by tests to drain before cleanup.
func (m *Manager) WaitForBackgroundWork() {
	m.goroutineWg.Wait()
}
