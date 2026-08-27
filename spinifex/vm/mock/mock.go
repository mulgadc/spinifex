// Package mock provides a single shared in-memory fake for vm.StateStore,
// reused by every package that stands up a stopped/terminated instance
// store in its tests. It also structurally satisfies the narrower
// handlers/ec2/instance.StoppedInstanceStore (see mock_test.go for the
// compile-time check, kept out of this file to avoid importing that
// package from here).
package mock

import (
	"errors"
	"fmt"
	"maps"
	"sync"

	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// StateStore is an in-memory vm.StateStore. Per-method error fields let a
// test drive a specific failure branch without a real KV backend; the
// Wrote*/Deleted*/Claimed* fields record every successful call, so a test
// can assert on call history independent of final state. Stopped is the
// claimable pool: Delete/Claim remove from it, but WriteStoppedInstance
// records only, it never puts a record back.
type StateStore struct {
	mu sync.Mutex

	Saved      map[string]map[string]*vm.VM
	Stopped    map[string]*vm.VM
	Terminated map[string]*vm.VM

	WroteStopped    map[string]*vm.VM
	WroteTerminated map[string]*vm.VM
	DeletedStopped  []string
	ClaimedStopped  []string

	SaveRunningErr     error
	LoadStoppedErr     error
	WriteStoppedErr    error
	UpdateStoppedErr   error
	ListStoppedErr     error
	DeleteStoppedErr   error
	ClaimStoppedErr    error
	WriteTerminatedErr error
	ListTerminatedErr  error

	// DeleteFailFirst makes the first DeleteStoppedInstance call fail and the
	// second succeed, exercising a caller's single retry.
	DeleteFailFirst bool
	DeleteAttempts  int

	// ClaimAfterLoad simulates a winning ClaimStoppedInstance landing between
	// a caller's Load and its later mutation: the record is removed right
	// after LoadStoppedInstance returns it.
	ClaimAfterLoad bool
	ClaimAttempts  int

	UpdateStoppedCalls int

	// OnSaveRunning fires after a successful SaveRunningState, outside the
	// lock, so a test can observe a save without polling a racy shared map.
	OnSaveRunning func()

	// OnWriteStoppedInstance fires after a successful WriteStoppedInstance,
	// outside the lock, so a test can inject a concurrent mutation (e.g. a
	// slot reclaim) at the same point production code would race it.
	OnWriteStoppedInstance func(id string, v *vm.VM)
}

// New returns a StateStore with all maps initialized.
func New() *StateStore {
	return &StateStore{
		Saved:      map[string]map[string]*vm.VM{},
		Stopped:    map[string]*vm.VM{},
		Terminated: map[string]*vm.VM{},
	}
}

var _ vm.StateStore = (*StateStore)(nil)

func (s *StateStore) SaveRunningState(nodeID string, snap map[string]*vm.VM) error {
	s.mu.Lock()
	if s.SaveRunningErr != nil {
		s.mu.Unlock()
		return s.SaveRunningErr
	}
	cp := make(map[string]*vm.VM, len(snap))
	maps.Copy(cp, snap)
	if s.Saved == nil {
		s.Saved = map[string]map[string]*vm.VM{}
	}
	s.Saved[nodeID] = cp
	hook := s.OnSaveRunning
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

func (s *StateStore) LoadRunningState(nodeID string) (map[string]*vm.VM, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.Saved[nodeID]; ok {
		return v, true, nil
	}
	return map[string]*vm.VM{}, false, nil
}

func (s *StateStore) WriteStoppedInstance(id string, v *vm.VM) error {
	s.mu.Lock()
	if s.WriteStoppedErr != nil {
		s.mu.Unlock()
		return s.WriteStoppedErr
	}
	// Records the write without repopulating Stopped: that map is the pool
	// ClaimStoppedInstance draws from, and a rollback write must not make an
	// already-claimed instance claimable a second time.
	if s.WroteStopped == nil {
		s.WroteStopped = map[string]*vm.VM{}
	}
	s.WroteStopped[id] = v
	hook := s.OnWriteStoppedInstance
	s.mu.Unlock()
	if hook != nil {
		hook(id, v)
	}
	return nil
}

func (s *StateStore) LoadStoppedInstance(id string) (*vm.VM, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.LoadStoppedErr != nil {
		return nil, s.LoadStoppedErr
	}
	v, ok := s.Stopped[id]
	if !ok {
		return nil, nil
	}
	if s.ClaimAfterLoad {
		delete(s.Stopped, id)
		s.ClaimedStopped = append(s.ClaimedStopped, id)
	}
	return v, nil
}

// DeleteStoppedInstance mimics a transient-then-success retry (DeleteFailFirst)
// and a hard injected error (DeleteStoppedErr); the two knobs are mutually
// exclusive in practice, DeleteFailFirst is checked first.
func (s *StateStore) DeleteStoppedInstance(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.DeleteAttempts++
	attempt := s.DeleteAttempts
	if s.DeleteFailFirst && attempt == 1 {
		return errors.New("simulated transient delete failure")
	}
	if s.DeleteStoppedErr != nil {
		return s.DeleteStoppedErr
	}
	delete(s.Stopped, id)
	s.DeletedStopped = append(s.DeletedStopped, id)
	return nil
}

func (s *StateStore) ListStoppedInstances() ([]*vm.VM, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ListStoppedErr != nil {
		return nil, s.ListStoppedErr
	}
	out := make([]*vm.VM, 0, len(s.Stopped))
	for _, v := range s.Stopped {
		out = append(out, v)
	}
	return out, nil
}

// ClaimStoppedInstance mimics the real atomic delete-as-claim: under the
// store lock, remove and return Stopped[id], or ClaimStoppedErr /
// vm.ErrStoppedInstanceClaimed if it is already gone.
func (s *StateStore) ClaimStoppedInstance(id string) (*vm.VM, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ClaimAttempts++
	if s.ClaimStoppedErr != nil {
		return nil, s.ClaimStoppedErr
	}
	v, ok := s.Stopped[id]
	if !ok {
		return nil, vm.ErrStoppedInstanceClaimed
	}
	delete(s.Stopped, id)
	s.ClaimedStopped = append(s.ClaimedStopped, id)
	return v, nil
}

// UpdateStoppedInstance mimics the real CAS semantics: mutate runs under the
// store lock against the stored value, and a missing record returns
// kvstore.ErrNotFound rather than resurrecting it.
func (s *StateStore) UpdateStoppedInstance(id string, mutate func(*vm.VM)) (*vm.VM, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.UpdateStoppedCalls++
	if s.UpdateStoppedErr != nil {
		return nil, s.UpdateStoppedErr
	}
	v, ok := s.Stopped[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", kvstore.ErrNotFound, id)
	}
	mutate(v)
	return v, nil
}

func (s *StateStore) WriteTerminatedInstance(id string, v *vm.VM) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.WriteTerminatedErr != nil {
		return s.WriteTerminatedErr
	}
	if s.Terminated == nil {
		s.Terminated = map[string]*vm.VM{}
	}
	s.Terminated[id] = v
	if s.WroteTerminated == nil {
		s.WroteTerminated = map[string]*vm.VM{}
	}
	s.WroteTerminated[id] = v
	return nil
}

func (s *StateStore) ListTerminatedInstances() ([]*vm.VM, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ListTerminatedErr != nil {
		return nil, s.ListTerminatedErr
	}
	out := make([]*vm.VM, 0, len(s.Terminated))
	for _, v := range s.Terminated {
		out = append(out, v)
	}
	return out, nil
}

func (s *StateStore) DeleteTerminatedInstance(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Terminated, id)
	return nil
}

// UpdateTerminatedInstance mimics the real CAS semantics: mutate runs under
// the store lock against the stored value.
func (s *StateStore) UpdateTerminatedInstance(id string, mutate func(*vm.VM)) (*vm.VM, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.Terminated[id]
	if !ok {
		return nil, errors.New("terminated instance not found")
	}
	mutate(v)
	return v, nil
}
