//test:in-package — ForgetSuperseded is asserted against Manager's own map and
//the in-package fake deps, which is where the rest of the shutdown suite lives.

package vm

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A returning node has to drop the instances the cluster moved while it was
// away, before its restore relaunches them from its own state file.
func TestForgetSuperseded_RemovesTheLocalCopy(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	m := NewManager()
	m.SetDeps(Deps{NodeID: "node-1", StateStore: newFakeStateStore()})

	instance := &VM{ID: "i-moved", InstanceType: "t3.micro"}
	m.Insert(instance)
	_, present := m.Get("i-moved")
	require.True(t, present)

	m.ForgetSuperseded(instance)

	_, present = m.Get("i-moved")
	assert.False(t, present,
		"an instance another node owns now must not survive in this node's view")
}

// Called on whatever the caller found, and what it found may be nothing.
func TestForgetSuperseded_NilIsANoOp(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	m := NewManager()

	assert.NotPanics(t, func() { m.ForgetSuperseded(nil) })
}

// Dropping the guest is the easy half. The export outlives it and holds the
// volume lease, and this node is healthy, so it would renew that lease forever —
// leaving the instance stopped here and unable to start anywhere.
func TestForgetSuperseded_GivesUpTheVolumesWithoutSealingThem(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	mounter := &fakeVolumeMounter{}
	m := NewManager()
	m.SetDeps(Deps{NodeID: "node-1", StateStore: newFakeStateStore(), VolumeMounter: mounter})

	instance := &VM{ID: "i-moved", InstanceType: "t3.micro"}
	instance.EBSRequests.Requests = []types.EBSRequest{{Name: "vol-root"}}
	m.Insert(instance)

	m.ForgetSuperseded(instance)

	assert.Equal(t, []string{"i-moved"}, mounter.abandoned,
		"the export has to be given up, or the new owner waits on a lease this node keeps renewing")
	assert.Empty(t, mounter.unmounted,
		"an unmount seals, and this node's block map is the stale one: sealing publishes it over the winner's")
}

// An export this node could not let go of is one the new owner cannot open, so
// the failure is reported rather than swallowed — but the instance still leaves,
// because it is running somewhere else either way.
func TestForgetSuperseded_DropsTheInstanceEvenIfTheVolumesRefuse(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	mounter := &fakeVolumeMounter{abandonErr: assert.AnError}
	m := NewManager()
	m.SetDeps(Deps{NodeID: "node-1", StateStore: newFakeStateStore(), VolumeMounter: mounter})

	instance := &VM{ID: "i-moved", InstanceType: "t3.micro"}
	instance.EBSRequests.Requests = []types.EBSRequest{{Name: "vol-root"}}
	m.Insert(instance)

	m.ForgetSuperseded(instance)

	assert.Equal(t, []string{"i-moved"}, mounter.abandoned)
	_, present := m.Get("i-moved")
	assert.False(t, present,
		"a volume that would not let go does not make the instance this node's again")
}

// The logical port is bound to whichever chassis offers a tap carrying its
// iface-id, and the binding names one chassis. Two nodes offering the same
// iface-id contend for it, so the address answers from whichever won last —
// which is the one failure a load balancer's customers would actually see.
func TestForgetSuperseded_StopsClaimingTheInstanceLogicalPort(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	plumber := &fakeNetworkPlumber{}
	cleaner := &recordingInstanceCleaner{}
	m := NewManager()
	m.SetDeps(Deps{
		NodeID: "node-1", StateStore: newFakeStateStore(),
		NetworkPlumber: plumber, InstanceCleaner: cleaner,
	})

	instance := &VM{ID: "i-moved", InstanceType: "t3.micro", ENIId: "eni-abc"}
	m.Insert(instance)

	m.ForgetSuperseded(instance)

	assert.Contains(t, plumber.cleanupCalls, TapDeviceName("eni-abc"),
		"a tap left behind keeps this node contending for a logical port another node now serves")
	assert.Equal(t, []string{"eni-abc"}, plumber.imdsDetachCalls,
		"the IMDS patch port claims the same iface-id, so it has to go with the tap")
	assert.Equal(t, []string{"i-moved"}, cleaner.cleanupMgmt,
		"the mgmt tap is this node's own and nothing else will remove it")
}

// The instance is not being terminated, only forgotten here. Anything that acts
// on the cluster's record of it would be this node deciding for the node that
// runs it now.
func TestForgetSuperseded_TouchesNothingTheNewOwnerOwns(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	cleaner := &recordingInstanceCleaner{}
	m := NewManager()
	m.SetDeps(Deps{NodeID: "node-1", StateStore: newFakeStateStore(), InstanceCleaner: cleaner})

	instance := &VM{ID: "i-moved", InstanceType: "t3.micro", ENIId: "eni-abc", PublicIP: "203.0.113.5"}
	m.Insert(instance)

	m.ForgetSuperseded(instance)

	assert.Empty(t, cleaner.deleteVolumes, "the volumes belong to the instance, and the instance moved")
	assert.Empty(t, cleaner.releasePublicIP, "the address follows the instance; releasing it here takes it away")
	assert.Empty(t, cleaner.detachAndDeleteENI, "the ENI is what the new owner plugs its own tap into")
}
