package vm

import (
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gcRacingCleaner drives an instance to terminated from inside the cleanup
// chain, standing in for the VM GC reconciling the same instance while a
// terminate is in flight.
type gcRacingCleaner struct {
	recordingInstanceCleaner

	onReleaseGPU func(v *VM)
}

func (c *gcRacingCleaner) ReleaseGPU(v *VM) error {
	if c.onReleaseGPU != nil {
		c.onReleaseGPU(v)
	}
	return c.recordingInstanceCleaner.ReleaseGPU(v)
}

// TestTerminate_GCFinalisedDuringCleanup_Converges covers the race observed on
// wattle: the GC finalises the instance while Terminate is inside
// terminateCleanup, so finalizeTerminated's precheck sees terminated ->
// terminated. Reaching the state we were driving to is convergence, not
// failure — returning an error there aborted the caller's own teardown
// (an Ochre endpoint delete) partway and stranded its record.
func TestTerminate_GCFinalisedDuringCleanup_Converges(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	store := newFakeStateStore()
	rt := &recordedTransitions{}
	cleaner := &gcRacingCleaner{}
	m := NewManager()
	rt.bind(m)
	m.SetDeps(Deps{
		NodeID:          "test-node",
		StateStore:      store,
		VolumeMounter:   &fakeVolumeMounter{},
		InstanceCleaner: cleaner,
		TransitionState: rt.apply,
		ShutdownSignal:  func() bool { return false },
	})

	v := &VM{ID: "i-gc-raced", Status: StateRunning, InstanceType: "t3.micro", Instance: &ec2.Instance{}}
	m.Insert(v)
	cleaner.onReleaseGPU = func(raced *VM) {
		m.Inspect(raced, func(x *VM) { x.Status = StateTerminated })
	}

	require.NoError(t, m.Terminate(v.ID), "a concurrent finalisation must not fail the terminate")

	assert.Equal(t, StateTerminated, m.Status(v))
	require.NotNil(t, store.terminated[v.ID], "the durable record must still land")
	_, stillInMap := m.Get(v.ID)
	assert.False(t, stillInMap, "the instance must leave the local map")
}

// TestFinalizeTerminated_GenuineInvalidTransitionStillFails keeps the guard
// narrow: only an instance that actually reached terminated converges. One
// that did not is still a real refusal.
func TestFinalizeTerminated_GenuineInvalidTransitionStillFails(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	store := newFakeStateStore()
	rt := &recordedTransitions{}
	m := NewManager()
	rt.bind(m)
	m.SetDeps(Deps{
		NodeID:          "test-node",
		StateStore:      store,
		VolumeMounter:   &fakeVolumeMounter{},
		InstanceCleaner: &recordingInstanceCleaner{},
		TransitionState: rt.apply,
		ShutdownSignal:  func() bool { return false },
	})

	v := &VM{ID: "i-not-terminated", Status: StatePending, Instance: &ec2.Instance{}}
	m.Insert(v)

	err := m.finalizeTerminated(v)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidTransition)
	assert.Nil(t, store.terminated[v.ID], "nothing durable may be recorded for a refused transition")
}
