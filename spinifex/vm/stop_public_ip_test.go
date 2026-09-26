// Drives Stop and StopAll against shutdownTestManager, whose fakes are
// unexported, and turns on DesiredState — the field that separates an operator
// stop from a host drain and decides whether the address is released.
//
//test:in-package — no exported seam reaches Manager.stopOne.
package vm

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stoppableWithAddress returns a running VM holding an auto-assigned address,
// already in m, shaped so stopOne takes the operator-stop path.
func stoppableWithAddress(t *testing.T, m *Manager, id string) *VM {
	t.Helper()
	v := &VM{
		ID:           id,
		Status:       StateRunning,
		InstanceType: "t3.micro",
		Instance:     &ec2.Instance{},
		DesiredState: DesiredStopped,
		ENIId:        "eni-1",
		PublicIP:     "203.0.113.10",
		PublicIPPool: "wan",
	}
	m.Insert(v)
	return v
}

// An auto-assigned address is borrowed for as long as the instance runs, so a
// stop hands it back and the record that migrates to the stopped bucket must
// not still advertise it.
func TestAnOperatorStopReturnsAnAutoAssignedAddressToItsPool(t *testing.T) {
	m, store, _, cleaner, _ := shutdownTestManager(t)
	cleaner.releaseAutoAssignedOK = true
	v := stoppableWithAddress(t, m, "i-auto")

	require.NoError(t, m.Stop(v.ID))

	assert.Equal(t, []string{"i-auto"}, cleaner.releaseAutoAssigned,
		"stop did not offer the address back to its pool")
	stored, err := store.LoadStoppedInstance("i-auto")
	require.NoError(t, err)
	require.NotNil(t, stored, "stopped instance never reached the shared bucket")
	assert.Empty(t, stored.PublicIP,
		"a stopped instance still advertises an address that went back to the pool")
	assert.Empty(t, stored.PublicIPPool)
	assert.True(t, stored.AutoAssignPublicIP,
		"the start that follows has nothing telling it to take a new address")
}

// An Elastic IP is the customer's and outlives every stop. The cleaner is what
// tells the two apart, so a refusal here has to leave the VM exactly as it was.
func TestAStopKeepsAnAddressTheCleanerRefusesToRelease(t *testing.T) {
	m, store, _, cleaner, _ := shutdownTestManager(t)
	cleaner.releaseAutoAssignedOK = false
	v := stoppableWithAddress(t, m, "i-eip")

	require.NoError(t, m.Stop(v.ID))

	stored, err := store.LoadStoppedInstance("i-eip")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "203.0.113.10", stored.PublicIP,
		"a stop took an Elastic IP off the instance it is associated with")
	assert.Equal(t, "wan", stored.PublicIPPool)
	assert.False(t, stored.AutoAssignPublicIP,
		"the start would auto-assign a second address beside the Elastic IP")
}

// A release that fails leaves the address where it is rather than clearing the
// record: an instance that reports no address while the pool still holds one is
// an address nothing will ever reclaim.
func TestAFailedReleaseLeavesTheAddressOnTheInstance(t *testing.T) {
	m, store, _, cleaner, _ := shutdownTestManager(t)
	cleaner.releaseAutoAssignedErr = errors.New("pool unreachable")
	v := stoppableWithAddress(t, m, "i-failed")

	require.NoError(t, m.Stop(v.ID))

	stored, err := store.LoadStoppedInstance("i-failed")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "203.0.113.10", stored.PublicIP,
		"the instance forgot an address the pool never took back")
}

// A host drain is not a customer stop. Restore relaunches the guest on the next
// boot, and changing its address under it would be a visible outage nobody
// asked for.
func TestAHostDrainKeepsTheAddress(t *testing.T) {
	m, _, _, cleaner, _ := shutdownTestManager(t)
	cleaner.releaseAutoAssignedOK = true
	v := stoppableWithAddress(t, m, "i-drain")
	v.DesiredState = DesiredRunning

	require.NoError(t, m.StopAll())

	assert.Empty(t, cleaner.releaseAutoAssigned,
		"a drain released the address of a guest it is about to relaunch")
	live, ok := m.Get("i-drain")
	require.True(t, ok)
	assert.Equal(t, "203.0.113.10", live.PublicIP)
}

// An instance with no public address has nothing to hand back, so the pool is
// never asked — the common case for a private-subnet guest.
func TestAStopWithNoAddressAsksThePoolNothing(t *testing.T) {
	m, _, _, cleaner, _ := shutdownTestManager(t)
	v := stoppableWithAddress(t, m, "i-private")
	v.PublicIP = ""
	v.PublicIPPool = ""

	require.NoError(t, m.Stop(v.ID))

	assert.Empty(t, cleaner.releaseAutoAssigned)
}
