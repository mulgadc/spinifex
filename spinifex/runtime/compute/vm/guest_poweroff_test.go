package vm

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/qmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func guestPoweroffManager(t *testing.T) (*Manager, *fakeStateStore, *recordedTransitions, *[]string) {
	t.Helper()
	m, store, _, _, rt := shutdownTestManager(t)
	var (
		mu   sync.Mutex
		down []string
	)
	m.SetDeps(Deps{
		NodeID:          "test-node",
		StateStore:      store,
		VolumeMounter:   &fakeVolumeMounter{},
		InstanceCleaner: &recordingInstanceCleaner{},
		TransitionState: rt.apply,
		ShutdownSignal:  func() bool { return false },
		CrashHandler:    m.HandleCrash,
		Hooks: ManagerHooks{
			OnInstanceDown: func(id string) {
				mu.Lock()
				down = append(down, id)
				mu.Unlock()
			},
		},
	})
	return m, store, rt, &down
}

func TestQMPHeartbeatPoll_MarksGuestPoweroff(t *testing.T) {
	for name, tc := range map[string]struct {
		status  string
		running bool
		want    bool
	}{
		"guest shut down": {qmpStatusShutdown, false, true},
		"guest running":   {"running", true, false},
	} {
		t.Run(name, func(t *testing.T) {
			qmpClient, cancel := newMockQMPClient(t, func(cmd qmp.QMPCommand) map[string]any {
				if cmd.Execute == "query-status" {
					return qmpStatusResponse(tc.status, tc.running)
				}
				return nil
			})
			defer cancel()

			m := NewManager()
			instance := &VM{ID: "i-1", Status: StateRunning, QMPClient: qmpClient}
			m.Insert(instance)

			require.True(t, m.qmpHeartbeatPoll(instance))
			assert.Equal(t, tc.want, instance.guestPoweroff.Load())
		})
	}
}

// A guest that powered itself off is a stop: stopping then stopped, released
// to the stopped bucket so StartInstances can bring it back, and never a crash.
func TestHandleQEMUExit_GuestPoweroffStopsInstance(t *testing.T) {
	m, store, rt, down := guestPoweroffManager(t)
	scheduled := countScheduledRestarts(t)

	instance := &VM{
		ID:           "i-poweroff",
		Status:       StateRunning,
		InstanceType: "t3.micro",
		Instance:     &ec2.Instance{},
	}
	instance.guestPoweroff.Store(true)
	m.Insert(instance)

	started := time.Now()
	m.HandleQEMUExit(instance, nil)

	assert.Less(t, time.Since(started), qmpHeartbeatInterval,
		"the stop must not wait for the next heartbeat")
	assert.Equal(t, []InstanceState{StateStopping, StateStopped}, rt.targets(instance.ID),
		"a guest poweroff goes stopping then stopped and never through error")
	assert.Zero(t, scheduled.Load(), "a guest poweroff must not be restarted")
	assert.Equal(t, []string{instance.ID}, *down)
	stored, err := store.LoadStoppedInstance(instance.ID)
	require.NoError(t, err)
	require.NotNil(t, stored, "the instance must be startable from the stopped bucket")
	assert.Equal(t, StateStopped, stored.Status)
}

// Without the guest-poweroff mark a clean exit is not ours to call a stop.
func TestHandleQEMUExit_CleanExitWithoutMarkIsLeftToCrashRecovery(t *testing.T) {
	m, _, rt, _ := guestPoweroffManager(t)

	instance := &VM{ID: "i-clean", Status: StateRunning, InstanceType: "t3.micro", Instance: &ec2.Instance{}}
	m.Insert(instance)

	m.HandleQEMUExit(instance, nil)

	assert.Empty(t, rt.targets(instance.ID), "no stop without the mark")
	assert.Equal(t, StateRunning, m.Status(instance))
}

// A wait error is a crash even when the mark is set: the mark only vouches for
// the quit we sent, not for a process that died some other way.
func TestHandleQEMUExit_WaitErrorIsACrashEvenWithMark(t *testing.T) {
	m, _, rt, _ := guestPoweroffManager(t)
	countScheduledRestarts(t)

	instance := &VM{ID: "i-killed", Status: StateRunning, InstanceType: "t3.micro", Instance: &ec2.Instance{}}
	instance.guestPoweroff.Store(true)
	m.Insert(instance)

	m.HandleQEMUExit(instance, errors.New("signal: killed"))

	assert.Contains(t, rt.targets(instance.ID), StateError)
	assert.NotContains(t, rt.targets(instance.ID), StateStopped)
}
