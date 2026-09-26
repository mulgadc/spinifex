package handlers_ec2_instance

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/vm"
	vmmock "github.com/mulgadc/spinifex/spinifex/vm/mock"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// refusingMounter fails the launch at its first real step, which is the
// earliest point past the address work these tests are about. Standing up a
// launchable guest would prove nothing extra and would need QEMU.
type refusingMounter struct{}

func (refusingMounter) Mount(context.Context, *vm.VM) error   { return errors.New("no volumes here") }
func (refusingMounter) Unmount(context.Context, *vm.VM) error { return nil }
func (refusingMounter) MountOne(context.Context, string, *types.EBSRequest) error {
	return nil
}
func (refusingMounter) UnmountOne(context.Context, string, types.EBSRequest) error { return nil }

// startFixture builds a stopped instance whose address went back to the pool,
// wired to fakes for everything the re-assignment touches. vmMgr.Run always
// fails in this harness, so every case asserts what the start did to the
// address before the launch, plus whatever the failure hands back.
type startFixture struct {
	svc   *InstanceServiceImpl
	store *vmmock.StateStore
	ipam  *fakeIPAllocator
	rel   *fakePublicIPReleaser
	eni   *fakeENICreator
}

func newStartFixture(t *testing.T, stopped *vm.VM) *startFixture {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	store := &vmmock.StateStore{Stopped: map[string]*vm.VM{stopped.ID: stopped}}
	prov := &fakeResourceCapacityProvider{
		instanceTypes: map[string]*ec2.InstanceTypeInfo{
			"t3.micro": {InstanceType: aws.String("t3.micro")},
		},
	}
	eniCreator := &fakeENICreator{getENIByID: map[string]*ENIInfo{
		"eni-1": {
			NetworkInterfaceID: "eni-1",
			VpcID:              "vpc-1",
			PrivateIpAddress:   "10.0.0.5",
			MacAddress:         "02:00:00:00:00:01",
		},
	}}
	f := &startFixture{
		store: store,
		ipam:  &fakeIPAllocator{publicIP: "203.0.113.20", poolName: "wan"},
		rel:   &fakePublicIPReleaser{},
		eni:   eniCreator,
	}
	vmMgr := vm.NewManager()
	vmMgr.SetDeps(vm.Deps{VolumeMounter: refusingMounter{}})
	f.svc = &InstanceServiceImpl{
		stoppedStore: store,
		resourceMgr:  prov,
		vmMgr:        vmMgr,
		ipAllocator:  f.ipam,
		ipReleaser:   f.rel,
		eniCreator:   eniCreator,
		natsConn:     embeddedNATS(t),
	}

	for _, topic := range []string{"vpc.add-nat", "vpc.delete-nat"} {
		sub, err := f.svc.natsConn.Subscribe(topic, func(msg *nats.Msg) {
			_ = msg.Respond([]byte(`{"success":true}`))
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
	return f
}

func (f *startFixture) start(t *testing.T, id string) error {
	t.Helper()
	_, err := f.svc.StartStoppedInstance(context.Background(), &StartStoppedInstanceInput{InstanceID: id}, "acc")
	return err
}

// releasedOnStop is the shape the stop path leaves behind: no address, and the
// marker saying one is owed.
func releasedOnStop(id string) *vm.VM {
	return &vm.VM{
		ID:                 id,
		Status:             vm.StateStopped,
		AccountID:          "acc",
		InstanceType:       "t3.micro",
		ENIId:              "eni-1",
		AutoAssignPublicIP: true,
		Instance:           &ec2.Instance{VpcId: aws.String("vpc-1"), PrivateIpAddress: aws.String("10.0.0.5")},
	}
}

// AWS gives a restarted instance a different address than the one it stopped
// with, so a start has to take a new one rather than assume the old one is
// still there.
func TestAStartTakesANewAddressForAnInstanceThatReleasedOneOnStop(t *testing.T) {
	f := newStartFixture(t, releasedOnStop("i-restart"))

	require.Error(t, f.start(t, "i-restart"), "the harness has no runnable VM")

	assert.Equal(t, 1, f.ipam.calls, "start did not allocate a replacement address")
	assert.Equal(t, 1, f.eni.updateCalls-f.eni.clearCalls,
		"the ENI record never learned the new address, so the reconciler would build no NAT rule")
}

// An instance that never had a public address must not be handed one by a
// start: a private-subnet guest is private on purpose.
func TestAStartLeavesAnInstanceThatNeverHadAnAddressAlone(t *testing.T) {
	stopped := releasedOnStop("i-private")
	stopped.AutoAssignPublicIP = false
	f := newStartFixture(t, stopped)

	require.Error(t, f.start(t, "i-private"))

	assert.Zero(t, f.ipam.calls, "start assigned a public address to an instance that had none")
}

// An Elastic IP associated while the instance was stopped is what it comes back
// on. Auto-assigning here would hand the customer a second address they never
// asked for and, on OCI, would bill them for it.
func TestAStartAdoptsAnElasticIPRatherThanAssigningBesideIt(t *testing.T) {
	f := newStartFixture(t, releasedOnStop("i-eip"))
	f.eni.eniHasEIP = true
	f.eni.getENIByID["eni-1"].PublicIpAddress = "198.51.100.7"
	f.eni.getENIByID["eni-1"].PublicIpPool = "wan"

	require.Error(t, f.start(t, "i-eip"))

	assert.Zero(t, f.ipam.calls, "start assigned a second address beside an Elastic IP")
	restored := f.store.WroteStopped["i-eip"]
	require.NotNil(t, restored)
	assert.Equal(t, "198.51.100.7", restored.PublicIP,
		"the instance does not report the Elastic IP it is reachable on")
}

// An exhausted pool is the AWS answer to a start that cannot be given an
// address, and the instance has to stay stopped and startable rather than be
// lost by the claim that already removed it.
func TestAStartAgainstAnExhaustedPoolIsRefusedAndTheInstanceSurvives(t *testing.T) {
	f := newStartFixture(t, releasedOnStop("i-exhausted"))
	f.ipam.err = errors.New(awserrors.ErrorInsufficientAddressCapacity)

	err := f.start(t, "i-exhausted")

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInsufficientAddressCapacity, err.Error())
	restored := f.store.WroteStopped["i-exhausted"]
	require.NotNil(t, restored, "the claim removed the instance and nothing put it back")
	assert.Equal(t, vm.StateStopped, restored.Status)
	assert.True(t, restored.AutoAssignPublicIP,
		"a retry would start the instance with no address at all")
}

// A launch that fails after the address was taken must hand it back, or every
// refused start costs the pool a slot.
func TestAFailedLaunchReturnsTheAddressTheStartTook(t *testing.T) {
	f := newStartFixture(t, releasedOnStop("i-launch-fail"))

	require.Error(t, f.start(t, "i-launch-fail"))

	assert.Equal(t, []string{"203.0.113.20"}, f.rel.released,
		"the address the start took was not returned when the launch failed")
	restored := f.store.WroteStopped["i-launch-fail"]
	require.NotNil(t, restored)
	assert.Empty(t, restored.PublicIP, "the restored record holds an address the pool has back")
	assert.True(t, restored.AutoAssignPublicIP, "the next start would bring the instance up with no address")
}

// An adopted Elastic IP is not the start's to give back, so a failed launch
// must leave it exactly where it is.
func TestAFailedLaunchDoesNotReturnAnAdoptedElasticIP(t *testing.T) {
	f := newStartFixture(t, releasedOnStop("i-eip-fail"))
	f.eni.eniHasEIP = true
	f.eni.getENIByID["eni-1"].PublicIpAddress = "198.51.100.7"
	f.eni.getENIByID["eni-1"].PublicIpPool = "wan"

	require.Error(t, f.start(t, "i-eip-fail"))

	assert.Empty(t, f.rel.released,
		"a failed launch gave a customer's Elastic IP back to the pool")
}
