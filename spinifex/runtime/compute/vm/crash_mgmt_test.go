package vm

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gatedUnmountMounter holds Unmount until released and refuses every Mount,
// recording the order in which they ran.
type gatedUnmountMounter struct {
	fakeVolumeMounter

	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	events  []string
}

func (g *gatedUnmountMounter) Unmount(_ context.Context, _ *VM) error {
	close(g.entered)
	<-g.release
	g.record("unmounted")
	return nil
}

func (g *gatedUnmountMounter) Mount(_ context.Context, _ *VM) error {
	g.record("mount")
	return errors.New("mount refused by test")
}

func (g *gatedUnmountMounter) record(e string) {
	g.mu.Lock()
	g.events = append(g.events, e)
	g.mu.Unlock()
}

func (g *gatedUnmountMounter) seen() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.events...)
}

func newCrashMgmtRig(t *testing.T, mounter VolumeMounter) *mgmtRig {
	t.Helper()
	r := newMgmtRig(t)
	prevAfter := restartAfterFunc
	t.Cleanup(func() { restartAfterFunc = prevAfter })
	restartAfterFunc = func(time.Duration, func()) *time.Timer { return time.NewTimer(time.Hour) }
	rt := (&recordedTransitions{}).bind(r.m)
	r.m.SetDeps(Deps{NodeID: mgmtTestNode, InstanceCleaner: r.cleaner, VolumeMounter: mounter,
		TransitionState: rt.apply, MgmtLaunch: MgmtLaunchDeps{Dir: r.dir, Addresses: r.addrs}})
	return r
}

// A start that follows a crash must not mount the guest's volumes while the
// crash cleanup is still unmounting them for the launch that died.
// Verifies: ADR-0003:S7.
func TestHandleCrash_ReplacementMountWaitsForCrashUnmount(t *testing.T) {
	mounter := &gatedUnmountMounter{entered: make(chan struct{}), release: make(chan struct{})}
	r := newCrashMgmtRig(t, mounter)
	r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 1, State: MgmtLaunchRunning, PID: 4001, StartTime: 1})
	v := mgmtVM("i-1")
	v.Status = StateRunning
	v.mgmtGen.Store(1)
	r.m.Insert(v)

	crashed := make(chan struct{})
	go func() {
		defer close(crashed)
		r.m.HandleCrash(v, errors.New("qemu exited"))
	}()
	<-mounter.entered

	started := make(chan error, 1)
	go func() { started <- r.m.Start(context.Background(), "i-1") }()
	assert.Never(t, func() bool { return len(mounter.seen()) > 0 }, 300*time.Millisecond, 10*time.Millisecond,
		"the replacement mounted while the crash cleanup was unmounting")

	close(mounter.release)
	<-crashed
	require.Error(t, <-started)
	assert.Equal(t, []string{"unmounted", "mount"}, mounter.seen())
}

// A crash reported by a launch that has since been replaced leaves the
// replacement's volumes and state alone.
func TestHandleCrash_StaleGenerationLeavesTheReplacement(t *testing.T) {
	mounter := &gatedUnmountMounter{entered: make(chan struct{}), release: make(chan struct{})}
	close(mounter.release)
	r := newCrashMgmtRig(t, mounter)
	r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 2, State: MgmtLaunchRunning, PID: 4002, StartTime: 2})
	v := mgmtVM("i-1")
	v.Status = StateRunning
	v.mgmtGen.Store(1)
	r.m.Insert(v)

	r.m.HandleCrash(v, errors.New("qemu exited"))

	assert.Equal(t, StateRunning, r.m.Status(v))
	assert.Empty(t, mounter.seen(), "a stale crash must not unmount the replacement's volumes")
	assert.Zero(t, v.Health.CrashCount)
}
