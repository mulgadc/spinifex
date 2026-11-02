// Builds an InstanceServiceImpl field by field and calls the claim path
// directly, because a cross-node relaunch is not reachable from the exported
// surface: it is driven by the recovery reconciler, not by a request.
//
//test:in-package — the service's dependencies and launchClaimedInstance itself are unexported.
package instance

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// claimedRelaunchService builds the service and manager a cross-node recovery
// launch runs through, with hook the pre-relaunch hook under test.
//
// The launch itself is expected to fail: the manager's resolver is unwired, so
// nothing spawns a qemu process. What matters is what happened before it.
func claimedRelaunchService(t *testing.T, hook func(*vm.VM) error) (*InstanceServiceImpl, *fakeResourceCapacityProvider) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	instanceType := &ec2.InstanceTypeInfo{InstanceType: aws.String("t3.micro")}
	resources := &fakeResourceCapacityProvider{
		instanceTypes: map[string]*ec2.InstanceTypeInfo{"t3.micro": instanceType},
	}

	mgr := vm.NewManager()
	mgr.SetDeps(vm.Deps{
		NodeID:        "node-b",
		VolumeMounter: raceVolumeMounter{},
		Hooks:         vm.ManagerHooks{BeforeInstanceRelaunch: hook},
	})

	return &InstanceServiceImpl{resourceMgr: resources, vmMgr: mgr}, resources
}

// The record names on-host state by path, and this host is not the one that
// wrote it. A system instance's boot configuration is a set of files under the
// runtime directory, so a recovery that does not rebuild them hands QEMU paths
// that exist only on the node the instance is being recovered from.
//
// This is the whole reason a load balancer never came back from a host failure:
// the hook that rebuilds those files was reachable only from the same-node
// restart path.
func TestLaunchClaimedInstance_RebuildsThisNodesCopyOfTheBootState(t *testing.T) {
	var prepared []string
	svc, _ := claimedRelaunchService(t, func(v *vm.VM) error {
		prepared = append(prepared, v.ID)
		return nil
	})

	instance := &vm.VM{ID: "i-moved", AccountID: "acc", InstanceType: "t3.micro"}
	_ = svc.launchClaimedInstance(t.Context(), instance, func() {})

	assert.Equal(t, []string{"i-moved"}, prepared,
		"a recovery onto another node has to rebuild what the record only names, or the guest boots with none of it")
}

// A guest that cannot be given its boot state would come up unreachable, which
// is worse than not coming up: the records would say it recovered. So the
// refusal is a launch failure, with everything the attempt took put back.
func TestLaunchClaimedInstance_RefusesWhenTheBootStateCannotBeRebuilt(t *testing.T) {
	svc, resources := claimedRelaunchService(t, func(*vm.VM) error {
		return assert.AnError
	})

	undone := false
	instance := &vm.VM{ID: "i-moved", AccountID: "acc", InstanceType: "t3.micro"}
	err := svc.launchClaimedInstance(t.Context(), instance, func() { undone = true })

	require.Error(t, err, "a guest that cannot be given its boot state must not be reported as recovered")
	assert.True(t, undone, "the claim has to be given back, or no other node will try")
	assert.Len(t, resources.deallocated, 1, "the capacity the attempt took has to be returned")
	_, present := svc.vmMgr.Get("i-moved")
	assert.False(t, present, "a refused launch must not leave the instance in this node's view")
}

// A claimed instance was launched long before this node runs it: a stopped
// instance can be started a day later, and a recovery waits out its old
// node's heartbeat and volume lease first. The pending watchdog times the
// launch from LaunchTime, so the old one has it terminated while it boots.
func TestLaunchClaimedInstance_RestampsLaunchTime(t *testing.T) {
	var launchedAt time.Time
	svc, _ := claimedRelaunchService(t, func(v *vm.VM) error {
		launchedAt = aws.TimeValue(v.Instance.LaunchTime)
		return nil
	})

	before := time.Now()
	instance := &vm.VM{ID: "i-moved", AccountID: "acc", InstanceType: "t3.micro",
		Instance: &ec2.Instance{LaunchTime: aws.Time(before.Add(-24 * time.Hour))}}
	_ = svc.launchClaimedInstance(t.Context(), instance, func() {})

	assert.False(t, launchedAt.Before(before),
		"the relaunch must start the pending watchdog's clock again, or a launch older than %s is terminated as stuck", vm.PendingWatchdogTimeout)
}

// A claimed instance has run before: it was started and stopped, or recovered
// from a failed node. A failure of this launch must never be allowed to
// terminate it and delete its volumes (ADR-0003:S7).
func TestLaunchClaimedInstance_NeverAFirstLaunch(t *testing.T) {
	var firstLaunch []bool
	svc, _ := claimedRelaunchService(t, func(v *vm.VM) error {
		firstLaunch = append(firstLaunch, v.FirstLaunch)
		return nil
	})

	instance := &vm.VM{ID: "i-moved", AccountID: "acc", InstanceType: "t3.micro", FirstLaunch: true}
	_ = svc.launchClaimedInstance(t.Context(), instance, func() {})

	assert.Equal(t, []bool{false}, firstLaunch,
		"ADR-0003:S7: a claimed relaunch reached launch still marked as a first launch, so its failure could terminate it")
}
