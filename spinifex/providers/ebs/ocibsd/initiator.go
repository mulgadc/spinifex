package ocibsd

import (
	"context"
	"fmt"
	"sync"
)

// Target is the iSCSI target an OCI attachment exposes.
type Target struct {
	IQN  string
	IPv4 string
	Port int
}

// ByPathDevice is the udev path of the target's one LUN.
func ByPathDevice(t Target) string {
	return fmt.Sprintf("/dev/disk/by-path/ip-%s:%d-iscsi-%s-lun-1", t.IPv4, t.Port, t.IQN)
}

// Initiator manages this node's iSCSI sessions.
type Initiator interface {
	// Login opens a session to t, or keeps the one open, and returns the
	// block device once it exists.
	Login(ctx context.Context, t Target) (devicePath string, err error)
	// Logout closes any session to t and forgets it. With none it succeeds.
	Logout(ctx context.Context, t Target) error
	// Sessions lists the targets this node holds a session to.
	Sessions(ctx context.Context) ([]Target, error)
}

// FakeInitiator is an in-memory Initiator that records the sessions it holds
// and the logins and logouts it was asked for, in order.
type FakeInitiator struct {
	mu           sync.Mutex
	sessions     map[Target]bool
	failLogin    error
	failSessions error
	events       []string
}

var _ Initiator = (*FakeInitiator)(nil)

// NewFakeInitiator returns a FakeInitiator holding no sessions.
func NewFakeInitiator() *FakeInitiator {
	return &FakeInitiator{sessions: map[Target]bool{}}
}

// FailNextLogin makes the next Login return err and open nothing.
func (f *FakeInitiator) FailNextLogin(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failLogin = err
}

// FailSessions makes every Sessions call return err.
func (f *FakeInitiator) FailSessions(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failSessions = err
}

// SeedSession opens a session no Login made, as one left by an earlier run.
func (f *FakeInitiator) SeedSession(t Target) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[t] = true
}

// Open reports the targets with an open session.
func (f *FakeInitiator) Open() []Target {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Target, 0, len(f.sessions))
	for t := range f.sessions {
		out = append(out, t)
	}
	return out
}

// Events is every login and logout, as "login <iqn>" or "logout <iqn>".
func (f *FakeInitiator) Events() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func (f *FakeInitiator) Login(_ context.Context, t Target) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "login "+t.IQN)
	if err := f.failLogin; err != nil {
		f.failLogin = nil
		return "", err
	}
	f.sessions[t] = true
	return ByPathDevice(t), nil
}

func (f *FakeInitiator) Logout(_ context.Context, t Target) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "logout "+t.IQN)
	delete(f.sessions, t)
	return nil
}

func (f *FakeInitiator) Sessions(context.Context) ([]Target, error) {
	if err := func() error { f.mu.Lock(); defer f.mu.Unlock(); return f.failSessions }(); err != nil {
		return nil, err
	}
	return f.Open(), nil
}
