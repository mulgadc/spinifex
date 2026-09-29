package vm

//test:in-package — FenceVolume's decision is about unexported manager state
//(instance status, StateReason, the local VM map) with no exported surface.

import (
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// vmUsingVolume builds a running instance with volumeID attached, as a guest
// that viperblockd has an export open for would look.
func vmUsingVolume(id, volumeID string) *VM {
	return &VM{
		ID:          id,
		Status:      StateRunning,
		Instance:    &ec2.Instance{},
		EBSRequests: types.EBSRequests{Requests: []types.EBSRequest{{Name: volumeID}}},
	}
}

// TestFenceVolume_StopsTheGuestUsingTheVolume is the property the fence exists
// for on this side. viperblockd has already torn the export down, so the guest
// is running against a disk that is gone and its writes can never land.
func TestFenceVolume_StopsTheGuestUsingTheVolume(t *testing.T) {
	m := NewManagerWithDeps(Deps{NodeID: "node-a"})
	instance := vmUsingVolume("i-fenced", "vol-fenced")
	m.Insert(instance)

	got := m.FenceVolume(t.Context(), "vol-fenced", "lease moved to node-b")
	m.goroutineWg.Wait()

	assert.Equal(t, "i-fenced", got)
	assert.Equal(t, StateError, m.Status(instance),
		"a guest whose disk was taken away is not running, whatever the record says")
}

// TestFenceVolume_RecordsWhyItStopped covers what an operator sees afterwards.
// A guest in error with no reason is indistinguishable from a crash, and the
// distinction is the whole point: another node owns this volume now.
func TestFenceVolume_RecordsWhyItStopped(t *testing.T) {
	m := NewManagerWithDeps(Deps{NodeID: "node-a"})
	instance := vmUsingVolume("i-fencedreason", "vol-fencedreason")
	m.Insert(instance)

	m.FenceVolume(t.Context(), "vol-fencedreason", "lease moved to node-b")
	m.goroutineWg.Wait()

	require.NotNil(t, instance.Instance.StateReason)
	assert.Equal(t, fenceStateReasonCode, *instance.Instance.StateReason.Code)
	assert.Equal(t, "lease moved to node-b", *instance.Instance.StateReason.Message)
}

// TestFenceVolume_RecordsTheNodeItRanOn keeps placement working after a fence.
// Start prefers the node that last ran the instance, so a fence that did not
// stamp LastNode would lose the only hint about where its state is.
func TestFenceVolume_RecordsTheNodeItRanOn(t *testing.T) {
	m := NewManagerWithDeps(Deps{NodeID: "node-a"})
	instance := vmUsingVolume("i-fencedlastnode", "vol-fencedlastnode")
	m.Insert(instance)

	m.FenceVolume(t.Context(), "vol-fencedlastnode", "lease moved")
	m.goroutineWg.Wait()

	assert.Equal(t, "node-a", instance.LastNode)
}

// TestFenceVolume_NoLocalGuestIsNotAnError covers the ordinary case. A volume
// is usually fenced after the instance using it has already gone, and there is
// nothing left here to stop.
func TestFenceVolume_NoLocalGuestIsNotAnError(t *testing.T) {
	m := NewManagerWithDeps(Deps{NodeID: "node-a"})
	m.Insert(vmUsingVolume("i-other", "vol-somethingelse"))

	assert.Empty(t, m.FenceVolume(t.Context(), "vol-nobodyhas", "lease moved"))
}

// TestFenceVolume_LeavesATerminalInstanceAlone stops the fence undoing a
// teardown already in flight. Driving a shutting-down instance to error would
// strand its volumes and ENIs, which is worse than the state it came from.
func TestFenceVolume_LeavesATerminalInstanceAlone(t *testing.T) {
	for _, state := range []InstanceState{StateError, StateShuttingDown, StateTerminated} {
		t.Run(string(state), func(t *testing.T) {
			m := NewManagerWithDeps(Deps{NodeID: "node-a"})
			instance := vmUsingVolume("i-fencedterminal", "vol-fencedterminal")
			instance.Status = state
			m.Insert(instance)

			got := m.FenceVolume(t.Context(), "vol-fencedterminal", "lease moved")
			m.goroutineWg.Wait()

			assert.Equal(t, "i-fencedterminal", got)
			assert.Equal(t, state, m.Status(instance), "a terminal instance must not be re-driven")
			assert.Nil(t, instance.Instance.StateReason,
				"the reason belongs to the teardown already under way, not to this fence")
		})
	}
}

// TestInstanceUsingVolume_MatchesOnAnyAttachedVolume covers the lookup. A guest
// is fenced for its data disk as readily as its root, so a match on any
// attached volume has to count.
func TestInstanceUsingVolume_MatchesOnAnyAttachedVolume(t *testing.T) {
	m := NewManagerWithDeps(Deps{})
	instance := &VM{
		ID:     "i-multi",
		Status: StateRunning,
		EBSRequests: types.EBSRequests{Requests: []types.EBSRequest{
			{Name: "vol-root"}, {Name: "vol-data"},
		}},
	}
	m.Insert(instance)

	assert.Equal(t, instance, m.instanceUsingVolume("vol-data"))
	assert.Equal(t, instance, m.instanceUsingVolume("vol-root"))
	assert.Nil(t, m.instanceUsingVolume("vol-absent"))
}
