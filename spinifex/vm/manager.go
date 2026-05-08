package vm

import (
	"maps"
	"sync"
	"time"
)

// ManagerHooks are callbacks the manager fires synchronously on every
// running/terminated transition. The daemon uses these to drive per-instance
// NATS subscription/unsubscription. Hook fields may be nil; nil hooks are no-ops.
//
// OnInstanceUp returns an error so callers can distinguish a soft launch
// (subscribe failures only logged) from a reconnect (failures must roll back
// QMP and abort the reconnect). Callers that don't care about the error may
// ignore it.
type ManagerHooks struct {
	OnInstanceUp   func(*VM) error
	OnInstanceDown func(id string)
	// OnInstanceRecovering fires from Restore once per instance that is
	// about to be relaunched. The daemon subscribes only the command
	// topic (ec2.cmd.<id>) here so concurrent terminate commands can
	// reach this node while the relaunch is in flight; the subsequent
	// OnInstanceUp on launch success is idempotent and reinstalls both
	// the command and console subscriptions.
	OnInstanceRecovering func(*VM)
}

// Deps bundles every collaborator the manager uses to drive lifecycle.
// Fields may be nil where the manager does not yet require them; call sites
// guard against nil so partial wiring during construction is safe.
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

	// ConsumeCleanShutdownMarker reports whether the previous daemon run
	// recorded a clean shutdown marker for this node, deleting it as a
	// side effect. Restore uses the result to decide whether to wait
	// briefly for stale QEMU PIDs to die before classifying state. Nil
	// is treated as "no marker" (cautious recovery).
	ConsumeCleanShutdownMarker func() bool
}

// Manager owns the in-memory map of running VMs on this node and every
// lifecycle transition that mutates that map.
type Manager struct {
	mu   sync.Mutex
	vms  map[string]*VM
	deps Deps
}

// NewManager returns a Manager with no collaborators wired. Production code
// uses NewManagerWithDeps; tests that only exercise the in-memory map keep
// this convenience.
func NewManager() *Manager {
	return &Manager{vms: make(map[string]*VM)}
}

// NewManagerWithDeps returns a Manager wired with collaborators.
func NewManagerWithDeps(deps Deps) *Manager {
	return &Manager{vms: make(map[string]*VM), deps: deps}
}

// SetDeps replaces the manager's dependencies. The daemon constructs the
// manager early (before NATS / JetStream / network plumber are available)
// and calls SetDeps once those collaborators exist. Must not be called
// concurrently with lifecycle methods.
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
// Returns true if the delete happened. Used by stop/terminate handlers to guard
// against the slot being reclaimed by a concurrent start handler.
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

// ForEach calls fn for each VM under the manager lock. fn must not call back
// into Manager methods that take the lock — that will deadlock. For lock-free
// iteration, use Snapshot.
func (m *Manager) ForEach(fn func(*VM)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.vms {
		fn(v)
	}
}

// Snapshot returns the current set of VMs as a slice. Callers must not mutate
// the slice's *VM entries' map-related fields; the underlying VM pointers are
// shared with the manager.
func (m *Manager) Snapshot() []*VM {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*VM, 0, len(m.vms))
	for _, v := range m.vms {
		out = append(out, v)
	}
	return out
}

// SnapshotMap returns a copy of the id→VM map. Used by the state persistence
// adapter so serialization can happen without holding the manager lock.
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

// UpdateState looks up id and, if found, runs fn(v) under lock. Used for
// atomic check-then-mutate or read-under-lock on a single VM. Returns true
// if the VM was found and fn was invoked.
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

// Status returns v.Status under the manager lock. Replaces the dominant
// "Inspect to read Status" pattern with a typed accessor. Read-only — no
// membership check, so callers may pass a pointer to an instance no longer
// in the map (e.g. mid-cleanup) and still get a consistent read.
func (m *Manager) Status(v *VM) InstanceState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return v.Status
}

// Inspect runs fn(v) under the manager lock for an already-resolved VM
// pointer. Used by call sites that hold a *VM (e.g. from a NATS handler
// dispatch) and need to read or mutate its fields with the same memory-
// ordering guarantee as map-keyed access. Prefer UpdateState (membership-
// checked) for mutation and Status for the read-Status pattern; Inspect
// remains for closures that mutate fields on a possibly-orphaned VM.
func (m *Manager) Inspect(v *VM, fn func(*VM)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(v)
}

// View runs fn with the live id→VM map under the manager lock. fn must not
// mutate the map nor retain references after returning. Used by serialization
// paths that need every VM's fields to be stable for the duration of the
// encode (e.g. JSON marshal during state persistence).
func (m *Manager) View(fn func(map[string]*VM)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(m.vms)
}

// Replace bulk-resets the manager to the given set of VMs. The supplied map
// is copied; callers may mutate it after the call returns. Used by Restore
// after loading persisted state.
func (m *Manager) Replace(vms map[string]*VM) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vms = make(map[string]*VM, len(vms))
	maps.Copy(m.vms, vms)
}
