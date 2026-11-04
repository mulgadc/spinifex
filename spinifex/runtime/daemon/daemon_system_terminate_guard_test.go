package daemon

import (
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	ec2eip "github.com/mulgadc/spinifex/spinifex/domains/ec2/eip"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/systeminstance"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	"github.com/mulgadc/spinifex/spinifex/domains/network/external"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeOwnership answers Classify from a fixed table; an ID it does not know is
// Absent, and err, when set, fails every read. launch supplies the allocation
// and ENI recorded for a system VM.
type fakeOwnership struct {
	byID   map[string]systeminstance.Ownership
	launch map[string]systeminstance.Classification
	err    error
}

func (f fakeOwnership) Classify(id string) (systeminstance.Classification, error) {
	if f.err != nil {
		return systeminstance.Classification{}, f.err
	}
	cls := f.launch[id]
	if o, ok := f.byID[id]; ok {
		cls.Ownership = o
		return cls, nil
	}
	return systeminstance.Classification{Ownership: systeminstance.OwnershipAbsent}, nil
}

var _ systeminstance.OwnershipClassifier = fakeOwnership{}

// countTerminations makes every state transition observable, so a test can prove
// vmMgr.Terminate was never reached.
func countTerminations(d *Daemon) *atomic.Int32 {
	var n atomic.Int32
	d.vmMgr.SetDeps(vm.Deps{NodeID: d.node, TransitionState: func(*vm.VM, vm.InstanceState) error {
		n.Add(1)
		return nil
	}})
	return &n
}

// guardDaemon is a test daemon whose ownership answers come from real
// instance records in a real JetStream KV, as in production.
func guardDaemon(t *testing.T) *Daemon {
	t.Helper()
	d := createTestDaemon(t, sharedNATSURL)
	t.Cleanup(d.vmMgr.WaitForBackgroundWork)
	_, nc, _ := testutil.StartTestJetStream(t)
	var err error
	d.jsManager, err = NewJetStreamManager(nc)
	require.NoError(t, err)
	require.NoError(t, d.jsManager.InitKVBucket())
	require.NoError(t, d.jsManager.InitTerminatedInstanceBucket())
	return d
}

// recordLocation is where a test instance's record lives.
type recordLocation int

const (
	atLive recordLocation = iota
	atStopped
	atTerminated
	atNone
)

// stage records inst where loc says and, for a live one, in the running set.
func stage(t *testing.T, d *Daemon, inst *vm.VM, loc recordLocation) {
	t.Helper()
	switch loc {
	case atLive:
		d.vmMgr.Insert(inst)
		require.NoError(t, d.jsManager.WriteInstanceRecord(inst.ID, inst.Record()))
	case atStopped:
		require.NoError(t, d.jsManager.WriteStoppedInstance(inst.ID, inst))
	case atTerminated:
		require.NoError(t, d.jsManager.WriteTerminatedInstanceRecord(inst.ID, inst.Record()))
	case atNone:
	}
}

func TestS7_TerminateSystemInstanceRefusesCustomerInstance(t *testing.T) {
	type tc struct {
		name string
		inst *vm.VM
		loc  recordLocation
	}
	cases := []tc{
		{"running customer", &vm.VM{ID: "i-c-running", Status: vm.StateRunning, AccountID: testAccountID}, atLive},
		{"stopped customer", &vm.VM{ID: "i-c-stopped", Status: vm.StateStopped, AccountID: testAccountID}, atStopped},
		{"terminated customer", &vm.VM{ID: "i-c-terminated", Status: vm.StateTerminated, AccountID: testAccountID}, atTerminated},
		{"system manager in a customer account", &vm.VM{ID: "i-c-mgr", Status: vm.StateRunning, AccountID: testAccountID, ManagedBy: tags.ManagedByEKS}, atLive},
		{"system account without a manager", &vm.VM{ID: "i-c-nomgr", Status: vm.StateRunning, AccountID: awsidentifiers.GlobalAccountID}, atLive},
		{"system account, unknown manager", &vm.VM{ID: "i-c-unknown", Status: vm.StateRunning, AccountID: awsidentifiers.GlobalAccountID, ManagedBy: "somebody"}, atLive},
	}

	for _, route := range []string{"local", "wrapper", "routed", "nats launcher"} {
		t.Run(route, func(t *testing.T) {
			d := guardDaemon(t)
			terminated := countTerminations(d)
			eip := &sysInstReclaimingEIP{}
			d.eipService = eip
			launcher := elbv2domain.NewNATSSystemInstanceLauncher(d.natsConn, 5*time.Second)

			for _, c := range cases {
				stage(t, d, c.inst, c.loc)
				var err error
				switch route {
				case "local":
					err = d.terminateSystemInstanceLocal(c.inst.ID)
				case "wrapper":
					err = d.TerminateSystemInstance(c.inst.ID)
				case "routed":
					require.NoError(t, d.subscribeSystemTerminate(c.inst.ID))
					err = d.terminateSystemInstanceRemote(c.inst.ID)
				case "nats launcher":
					// The path ELBv2 takes: straight to the owning node's handler.
					require.NoError(t, d.subscribeSystemTerminate(c.inst.ID))
					err = launcher.TerminateSystemInstance(c.inst.ID)
				}
				require.Error(t, err, c.name)
				assert.Contains(t, err.Error(), systeminstance.ErrSystemInstanceNotSystem.Error(), c.name)
				assert.NotContains(t, err.Error(), systeminstance.ErrSystemInstanceNotFound.Error(), c.name)
				if route != "nats launcher" {
					require.ErrorIs(t, err, systeminstance.ErrSystemInstanceNotSystem, c.name)
					assert.NotErrorIs(t, err, systeminstance.ErrSystemInstanceNotFound, c.name)
				}
				if c.loc == atLive {
					assert.Equal(t, vm.StateRunning, d.vmMgr.Status(c.inst), c.name)
				}
			}
			assert.Zero(t, terminated.Load(), "vmMgr.Terminate is never reached for a customer instance")
			assert.Empty(t, eip.reclaimed, "a refused terminate must not run the address backstop")
			assert.Empty(t, eip.released)
		})
	}
}

func TestS7_TerminateSystemInstanceRefusesOnAbsentOrError(t *testing.T) {
	t.Run("absent id", func(t *testing.T) {
		d := guardDaemon(t)
		terminated := countTerminations(d)
		eip := &sysInstReclaimingEIP{}
		d.eipService = eip

		err := d.TerminateSystemInstance("i-nobody")

		require.ErrorIs(t, err, systeminstance.ErrSystemInstanceNotFound)
		assert.Zero(t, terminated.Load())
		assert.Empty(t, eip.reclaimed)
	})

	for name, setup := range map[string]func(*Daemon){
		"classifier error":  func(d *Daemon) { d.ownership = fakeOwnership{err: errInjected} },
		"no instance state": func(d *Daemon) { d.jsManager = nil },
	} {
		t.Run(name, func(t *testing.T) {
			d := guardDaemon(t)
			terminated := countTerminations(d)
			eip := &sysInstReclaimingEIP{}
			d.eipService = eip
			sys := &vm.VM{ID: "i-sys-unreadable", Status: vm.StateRunning, AccountID: awsidentifiers.GlobalAccountID, ManagedBy: tags.ManagedByELBv2}
			d.vmMgr.Insert(sys)
			setup(d)

			for _, err := range []error{d.TerminateSystemInstance(sys.ID), d.terminateSystemInstanceLocal(sys.ID)} {
				require.ErrorIs(t, err, systeminstance.ErrSystemInstanceOwnershipUnknown)
				assert.NotErrorIs(t, err, systeminstance.ErrSystemInstanceNotFound)
			}
			assert.Equal(t, vm.StateRunning, d.vmMgr.Status(sys), "the VM keeps running when ownership cannot be read")
			assert.Zero(t, terminated.Load())
			assert.Empty(t, eip.reclaimed)
		})
	}
}

// A routed owner that cannot read the record answers ownership-unknown, and the
// asking node must keep that distinct from the VM being gone.
func TestTerminateSystemInstance_LookupErrorIsNotNotFound(t *testing.T) {
	d := guardDaemon(t)
	d.ownership = fakeOwnership{err: errors.New("kv down")}
	require.NoError(t, d.subscribeSystemTerminate("i-lookup-err"))

	err := d.terminateSystemInstanceRemote("i-lookup-err")
	require.ErrorIs(t, err, systeminstance.ErrSystemInstanceOwnershipUnknown)
	assert.NotErrorIs(t, err, systeminstance.ErrSystemInstanceNotFound)

	err = elbv2domain.NewNATSSystemInstanceLauncher(d.natsConn, 5*time.Second).TerminateSystemInstance("i-lookup-err")
	require.Error(t, err)
	assert.Contains(t, err.Error(), systeminstance.ErrSystemInstanceOwnershipUnknown.Error())
	assert.NotContains(t, err.Error(), systeminstance.ErrSystemInstanceNotFound.Error())
}

func TestS7_TerminateSystemInstanceStillTerminatesEverySystemKind(t *testing.T) {
	for _, managedBy := range []string{tags.ManagedByELBv2, tags.ManagedByEKS, tags.ManagedByRDS, tags.ManagedByBedrock, tags.ManagedBySQS} {
		t.Run(managedBy, func(t *testing.T) {
			d := guardDaemon(t)

			local := &vm.VM{ID: "i-sys-local-" + managedBy, Status: vm.StateRunning, AccountID: awsidentifiers.GlobalAccountID, ManagedBy: managedBy}
			stage(t, d, local, atLive)
			require.NoError(t, d.TerminateSystemInstance(local.ID))
			assert.Equal(t, vm.StateTerminated, d.vmMgr.Status(local))

			routed := &vm.VM{ID: "i-sys-routed-" + managedBy, Status: vm.StateRunning, AccountID: awsidentifiers.GlobalAccountID, ManagedBy: managedBy}
			stage(t, d, routed, atLive)
			require.NoError(t, d.subscribeSystemTerminate(routed.ID))
			reply, err := d.natsConn.Request("system.TerminateInstance."+routed.ID, nil, 5*time.Second)
			require.NoError(t, err)
			var env systemInstanceTerminateEnvelope
			require.NoError(t, json.Unmarshal(reply.Data, &env))
			assert.Empty(t, env.Error)
			assert.Equal(t, vm.StateTerminated, d.vmMgr.Status(routed))
		})
	}
}

// bindRealEIP gives d a real EIP service and allocates an address, tags it as
// the customer chose, then records instanceID against it.
func bindRealEIP(t *testing.T, d *Daemon, instanceID string) (allocID string, svc *ec2eip.EIPServiceImpl) {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)
	ipam, err := ec2vpc.NewExternalIPAM(t.Context(), js, []external.ExternalPoolConfig{
		{Name: "wan-test", RangeStart: "203.0.113.10", RangeEnd: "203.0.113.20", Gateway: "203.0.113.1", PrefixLen: 24},
	})
	require.NoError(t, err)
	d.externalIPAM = ipam
	svc, err = ec2eip.NewEIPServiceImpl(t.Context(), nc, ipam, d.vpcService)
	require.NoError(t, err)
	d.eipService = svc

	out, err := svc.AllocateAddress(t.Context(), &ec2.AllocateAddressInput{
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String("elastic-ip"),
			Tags:         []*ec2.Tag{{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByELBv2)}},
		}},
	}, testAccountID)
	require.NoError(t, err)
	desc, err := svc.DescribeAddresses(t.Context(), &ec2.DescribeAddressesInput{AllocationIds: []*string{out.AllocationId}}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.Addresses, 1)
	require.Contains(t, desc.Addresses[0].Tags, &ec2.Tag{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByELBv2)},
		"the customer address carries the system manager tag")

	kv, err := js.KeyValue(t.Context(), ec2eip.KVBucketEIPs)
	require.NoError(t, err)
	key := testAccountID + "." + aws.StringValue(out.AllocationId)
	entry, err := kv.Get(t.Context(), key)
	require.NoError(t, err)
	var rec ec2eip.EIPRecord
	require.NoError(t, json.Unmarshal(entry.Value(), &rec))
	rec.InstanceId = instanceID
	data, err := json.Marshal(rec)
	require.NoError(t, err)
	_, err = kv.Update(t.Context(), key, data, entry.Revision())
	require.NoError(t, err)
	return aws.StringValue(out.AllocationId), svc
}

// associateEIP records allocID as associated with eniID, as AssociateAddress would.
func associateEIP(t *testing.T, kv jetstream.KeyValue, allocID, eniID string) {
	t.Helper()
	key := testAccountID + "." + allocID
	entry, err := kv.Get(t.Context(), key)
	require.NoError(t, err)
	var rec ec2eip.EIPRecord
	require.NoError(t, json.Unmarshal(entry.Value(), &rec))
	rec.State, rec.AssociationId, rec.ENIId = "associated", "eipassoc-"+allocID, eniID
	data, err := json.Marshal(rec)
	require.NoError(t, err)
	_, err = kv.Update(t.Context(), key, data, entry.Revision())
	require.NoError(t, err)
}

func TestS7_BackstopNeverReleasesCustomerAssociatedAddress(t *testing.T) {
	d := createVPCTestDaemon(t)
	_, nc, js := testutil.StartTestJetStream(t)
	var err error
	d.jsManager, err = NewJetStreamManager(nc)
	require.NoError(t, err)
	require.NoError(t, d.jsManager.InitKVBucket())
	require.NoError(t, d.jsManager.InitTerminatedInstanceBucket())

	ipam, err := ec2vpc.NewExternalIPAM(t.Context(), js, []external.ExternalPoolConfig{
		{Name: "wan-test", RangeStart: "203.0.113.10", RangeEnd: "203.0.113.20", Gateway: "203.0.113.1", PrefixLen: 24},
	})
	require.NoError(t, err)
	svc, err := ec2eip.NewEIPServiceImpl(t.Context(), nc, ipam, d.vpcService)
	require.NoError(t, err)
	d.eipService = svc
	kv, err := js.KeyValue(t.Context(), ec2eip.KVBucketEIPs)
	require.NoError(t, err)

	launch, err := svc.AllocateAddress(t.Context(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	customer, err := svc.AllocateAddress(t.Context(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	launchID, customerID := aws.StringValue(launch.AllocationId), aws.StringValue(customer.AllocationId)
	associateEIP(t, kv, launchID, "eni-alb")
	associateEIP(t, kv, customerID, "eni-alb")

	// No node owns the VM, so the wrapper's backstop is the only release.
	alb := &vm.VM{ID: "i-alb-backstop", Status: vm.StateTerminated, AccountID: awsidentifiers.GlobalAccountID,
		ManagedBy: tags.ManagedByELBv2, ENIId: "eni-alb", PublicIPAllocID: launchID}
	stage(t, d, alb, atTerminated)

	require.ErrorIs(t, d.TerminateSystemInstance(alb.ID), systeminstance.ErrSystemInstanceNotFound)
	assert.False(t, addressKept(t, svc, launchID), "the launch allocation is released")
	assert.True(t, addressKept(t, svc, customerID), "a customer address associated to the same ENI survives")
}

func addressKept(t *testing.T, svc *ec2eip.EIPServiceImpl, allocID string) bool {
	t.Helper()
	out, err := svc.DescribeAddresses(t.Context(), &ec2.DescribeAddressesInput{AllocationIds: []*string{aws.String(allocID)}}, testAccountID)
	return err == nil && len(out.Addresses) == 1
}

func TestS7_TerminateSystemInstanceNeverReleasesCustomerEIP(t *testing.T) {
	cases := map[string]struct {
		inst *vm.VM
		loc  recordLocation
	}{
		"customer record gone, address still names it": {&vm.VM{ID: "i-cust-gone", AccountID: testAccountID}, atNone},
		"customer stopped instance":                    {&vm.VM{ID: "i-cust-stopped-eip", Status: vm.StateStopped, AccountID: testAccountID}, atStopped},
		"customer terminated instance":                 {&vm.VM{ID: "i-cust-term-eip", Status: vm.StateTerminated, AccountID: testAccountID}, atTerminated},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			d := createVPCTestDaemon(t)
			_, nc, _ := testutil.StartTestJetStream(t)
			var err error
			d.jsManager, err = NewJetStreamManager(nc)
			require.NoError(t, err)
			require.NoError(t, d.jsManager.InitKVBucket())
			require.NoError(t, d.jsManager.InitTerminatedInstanceBucket())
			stage(t, d, c.inst, c.loc)
			allocID, svc := bindRealEIP(t, d, c.inst.ID)

			err = d.TerminateSystemInstance(c.inst.ID)

			if c.loc == atNone {
				require.ErrorIs(t, err, systeminstance.ErrSystemInstanceNotFound)
			} else {
				require.ErrorIs(t, err, systeminstance.ErrSystemInstanceNotSystem)
			}
			assert.True(t, addressKept(t, svc, allocID), "a customer's address survives a wrong system instance ID")
		})
	}
}

// flipOwnership classifies as a system VM once, then fails every later read, as
// an owner node does when its record store drops between the two lookups.
type flipOwnership struct {
	first systeminstance.Classification
	calls *atomic.Int32
}

func (f flipOwnership) Classify(string) (systeminstance.Classification, error) {
	if f.calls.Add(1) == 1 {
		return f.first, nil
	}
	return systeminstance.Classification{}, errors.New("kv down")
}

func TestS7_WrapperSkipsBackstopWhenOwnerCannotReadRecord(t *testing.T) {
	d := createVPCTestDaemon(t)
	_, nc, js := testutil.StartTestJetStream(t)
	ipam, err := ec2vpc.NewExternalIPAM(t.Context(), js, []external.ExternalPoolConfig{
		{Name: "wan-test", RangeStart: "203.0.113.10", RangeEnd: "203.0.113.20", Gateway: "203.0.113.1", PrefixLen: 24},
	})
	require.NoError(t, err)
	svc, err := ec2eip.NewEIPServiceImpl(t.Context(), nc, ipam, d.vpcService)
	require.NoError(t, err)
	d.eipService = svc
	kv, err := js.KeyValue(t.Context(), ec2eip.KVBucketEIPs)
	require.NoError(t, err)

	launch, err := svc.AllocateAddress(t.Context(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	launchID := aws.StringValue(launch.AllocationId)
	associateEIP(t, kv, launchID, "eni-alb")

	d.natsConn = nc
	d.ownership = flipOwnership{
		first: systeminstance.Classification{Ownership: systeminstance.OwnershipSystem, PublicIPAllocID: launchID, ENIID: "eni-alb"},
		calls: &atomic.Int32{},
	}
	require.NoError(t, d.subscribeSystemTerminate("i-alb-flip"))

	err = d.TerminateSystemInstance("i-alb-flip")

	require.ErrorIs(t, err, systeminstance.ErrSystemInstanceOwnershipUnknown)
	assert.True(t, addressKept(t, svc, launchID), "the VM may still be running, so its launch address is kept")
}

// A refused customer ID must never read as "gone": callers act on the ID after
// NotFound, and acting on a customer instance is the hole this guard closes.
func TestS7_NotSystemIsNeitherNotFoundNorOwnershipUnknown(t *testing.T) {
	assert.NotErrorIs(t, systeminstance.ErrSystemInstanceNotSystem, systeminstance.ErrSystemInstanceNotFound)
	assert.NotErrorIs(t, systeminstance.ErrSystemInstanceNotSystem, systeminstance.ErrSystemInstanceOwnershipUnknown)
	assert.NotContains(t, systeminstance.ErrSystemInstanceNotSystem.Error(), systeminstance.ErrSystemInstanceNotFound.Error())
	assert.NotContains(t, systeminstance.ErrSystemInstanceNotFound.Error(), systeminstance.ErrSystemInstanceNotSystem.Error())
}

// The asking node maps a routed customer refusal back to the same sentinel.
func TestTerminateSystemInstance_RoutedNotSystemMapsBack(t *testing.T) {
	d := guardDaemon(t)
	stage(t, d, &vm.VM{ID: "i-routed-cust", Status: vm.StateStopped, AccountID: testAccountID}, atStopped)
	require.NoError(t, d.subscribeSystemTerminate("i-routed-cust"))

	err := d.terminateSystemInstanceRemote("i-routed-cust")
	require.ErrorIs(t, err, systeminstance.ErrSystemInstanceNotSystem)
	assert.NotErrorIs(t, err, systeminstance.ErrSystemInstanceNotFound)
	assert.NotErrorIs(t, err, systeminstance.ErrSystemInstanceOwnershipUnknown)
}
