package daemon

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
)

// allInstanceStates is every state an instance can be persisted in. The table
// below covers all of them on purpose: before the cutover the key an instance
// sat under decided which set it was in, so no state could put it in the wrong
// one. Now one does, and an unconsidered state is how that goes wrong quietly.
var allInstanceStates = []vm.InstanceState{
	vm.StateProvisioning,
	vm.StatePending,
	vm.StateRunning,
	vm.StateStopping,
	vm.StateStopped,
	vm.StateShuttingDown,
	vm.StateTerminated,
	vm.StateError,
}

func record(status vm.InstanceState, desired vm.DesiredState, node string) *vm.InstanceRecord {
	return (&vm.VM{ID: "i-1", Status: status, DesiredState: desired, LastNode: node}).Record()
}

func TestOperatorStopped_OnlyStoppedAndAskedFor(t *testing.T) {
	for _, status := range allInstanceStates {
		for _, desired := range []vm.DesiredState{vm.DesiredRunning, vm.DesiredStopped} {
			want := status == vm.StateStopped && desired == vm.DesiredStopped
			assert.Equal(t, want, operatorStopped(record(status, desired, "node-1")),
				"status=%s desired=%q", status, desired)
		}
	}
}

// The case that makes the pair necessary rather than the status alone: a DRAIN
// stop leaves StateStopped with DesiredRunning, and restore relaunches it. Read
// as operator-stopped it would be listed as stopped and never come back.
func TestOperatorStopped_DrainStoppedIsNotOperatorStopped(t *testing.T) {
	drained := record(vm.StateStopped, vm.DesiredRunning, "node-1")
	assert.False(t, operatorStopped(drained))
	assert.True(t, runsOn(drained, "node-1"), "a drain-stopped instance is still the node's to relaunch")
}

func TestRunsOn_EverythingTheNodeOwnsExceptOperatorStopped(t *testing.T) {
	for _, status := range allInstanceStates {
		stopped := status == vm.StateStopped
		assert.Equal(t, !stopped, runsOn(record(status, vm.DesiredStopped, "node-1"), "node-1"),
			"desired-stopped, status=%s", status)
		assert.True(t, runsOn(record(status, vm.DesiredRunning, "node-1"), "node-1"),
			"desired-running, status=%s", status)
	}
}

// Terminated records stay in the running set until restore has moved them to
// the terminated bucket. Excluding them here is what its own comment calls a
// void: gone from the running set and not yet anywhere else.
func TestRunsOn_TerminatedIsStillTheNodesToMigrate(t *testing.T) {
	assert.True(t, runsOn(record(vm.StateTerminated, vm.DesiredRunning, "node-1"), "node-1"))
}

func TestRunsOn_AnotherNodesRecordIsNotOurs(t *testing.T) {
	assert.False(t, runsOn(record(vm.StateRunning, vm.DesiredRunning, "node-2"), "node-1"))
	assert.False(t, runsOn(record(vm.StateRunning, vm.DesiredRunning, ""), "node-1"))
	assert.False(t, runsOn(record(vm.StateRunning, vm.DesiredRunning, "node-1"), ""))
	assert.False(t, runsOn(nil, "node-1"))
	assert.False(t, operatorStopped(nil))
}
