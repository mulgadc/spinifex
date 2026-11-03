package daemon

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	ec2eip "github.com/mulgadc/spinifex/spinifex/domains/ec2/eip"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/systeminstance"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/domains/network/external"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func TestS7_TerminateSystemInstanceRefusesCustomerInstance(t *testing.T) {
	customers := map[string]*vm.VM{
		"customer":                 {ID: "i-cust-plain", Status: vm.StateRunning, AccountID: testAccountID},
		"system manager, customer": {ID: "i-cust-managed", Status: vm.StateRunning, AccountID: testAccountID, ManagedBy: tags.ManagedByEKS},
		"system account, no owner": {ID: "i-sys-unmanaged", Status: vm.StateRunning, AccountID: awsidentifiers.GlobalAccountID},
		"unknown manager":          {ID: "i-unknown-mgr", Status: vm.StateRunning, AccountID: awsidentifiers.GlobalAccountID, ManagedBy: "somebody"},
	}

	t.Run("local", func(t *testing.T) {
		d := createTestDaemon(t, sharedNATSURL)
		t.Cleanup(d.vmMgr.WaitForBackgroundWork)
		terminated := countTerminations(d)
		eip := &sysInstReclaimingEIP{}
		d.eipService = eip
		for name, inst := range customers {
			d.vmMgr.Insert(inst)
			err := d.terminateSystemInstanceLocal(inst.ID)
			require.ErrorIs(t, err, systeminstance.ErrSystemInstanceNotFound, name)
			err = d.TerminateSystemInstance(inst.ID)
			require.ErrorIs(t, err, systeminstance.ErrSystemInstanceNotFound, name)
			assert.Equal(t, vm.StateRunning, d.vmMgr.Status(inst), name)
		}
		assert.Zero(t, terminated.Load(), "vmMgr.Terminate is never reached for a customer instance")
		assert.Empty(t, eip.reclaimed, "a refused terminate must not release the instance's address")
	})

	t.Run("routed", func(t *testing.T) {
		d := createTestDaemon(t, sharedNATSURL)
		t.Cleanup(d.vmMgr.WaitForBackgroundWork)
		terminated := countTerminations(d)
		for name, inst := range customers {
			d.vmMgr.Insert(inst)
			require.NoError(t, d.subscribeSystemTerminate(inst.ID))
			reply, err := d.natsConn.Request("system.TerminateInstance."+inst.ID, nil, 5*time.Second)
			require.NoError(t, err, name)
			var env systemInstanceTerminateEnvelope
			require.NoError(t, json.Unmarshal(reply.Data, &env))
			assert.Contains(t, env.Error, systeminstance.ErrSystemInstanceNotFound.Error(), name)
			assert.Equal(t, vm.StateRunning, d.vmMgr.Status(inst), name)
		}
		assert.Zero(t, terminated.Load(), "vmMgr.Terminate is never reached for a customer instance")
	})
}

func TestS7_TerminateSystemInstanceStillTerminatesEverySystemKind(t *testing.T) {
	for _, managedBy := range []string{tags.ManagedByELBv2, tags.ManagedByEKS, tags.ManagedByRDS, tags.ManagedByBedrock, tags.ManagedBySQS} {
		t.Run(managedBy, func(t *testing.T) {
			d := createTestDaemon(t, sharedNATSURL)
			t.Cleanup(d.vmMgr.WaitForBackgroundWork)
			local := &vm.VM{ID: "i-sys-local-" + managedBy, Status: vm.StateRunning, AccountID: awsidentifiers.GlobalAccountID, ManagedBy: managedBy}
			d.vmMgr.Insert(local)
			require.NoError(t, d.TerminateSystemInstance(local.ID))
			assert.Equal(t, vm.StateTerminated, d.vmMgr.Status(local))

			routed := &vm.VM{ID: "i-sys-routed-" + managedBy, Status: vm.StateRunning, AccountID: awsidentifiers.GlobalAccountID, ManagedBy: managedBy}
			d.vmMgr.Insert(routed)
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

// bindRealEIP gives d a real EIP service and allocates an address, optionally
// tagged as system-managed, then records instanceID against it.
func bindRealEIP(t *testing.T, d *Daemon, instanceID, managedBy string) (allocID, publicIP string, svc *ec2eip.EIPServiceImpl) {
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

	in := &ec2.AllocateAddressInput{}
	if managedBy != "" {
		in.TagSpecifications = []*ec2.TagSpecification{{
			ResourceType: aws.String("elastic-ip"),
			Tags:         []*ec2.Tag{{Key: aws.String(tags.ManagedByKey), Value: aws.String(managedBy)}},
		}}
	}
	out, err := svc.AllocateAddress(t.Context(), in, testAccountID)
	require.NoError(t, err)

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
	return aws.StringValue(out.AllocationId), aws.StringValue(out.PublicIp), svc
}

func addressKept(t *testing.T, svc *ec2eip.EIPServiceImpl, allocID string) bool {
	t.Helper()
	out, err := svc.DescribeAddresses(t.Context(), &ec2.DescribeAddressesInput{AllocationIds: []*string{aws.String(allocID)}}, testAccountID)
	return err == nil && len(out.Addresses) == 1
}

func TestS7_TerminateSystemInstanceNeverReleasesCustomerEIP(t *testing.T) {
	t.Run("remote owner refuses", func(t *testing.T) {
		owner := createTestDaemon(t, sharedNATSURL)
		t.Cleanup(owner.vmMgr.WaitForBackgroundWork)
		cust := &vm.VM{ID: "i-cust-remote-eip", Status: vm.StateRunning, AccountID: testAccountID}
		owner.vmMgr.Insert(cust)
		require.NoError(t, owner.subscribeSystemTerminate(cust.ID))

		caller := createVPCTestDaemon(t)
		allocID, _, svc := bindRealEIP(t, caller, cust.ID, "")

		err := caller.TerminateSystemInstance(cust.ID)
		require.ErrorIs(t, err, systeminstance.ErrSystemInstanceNotFound)
		assert.Equal(t, vm.StateRunning, owner.vmMgr.Status(cust))
		assert.True(t, addressKept(t, svc, allocID), "a customer address survives a refused terminate")
	})

	t.Run("no owner, stopped customer instance", func(t *testing.T) {
		d := createVPCTestDaemon(t)
		id := fmt.Sprintf("i-cust-stopped-%d", time.Now().UnixNano())
		allocID, _, svc := bindRealEIP(t, d, id, "")

		_ = d.TerminateSystemInstance(id)
		assert.True(t, addressKept(t, svc, allocID), "a customer address survives a wrong system instance ID")
	})

	t.Run("system address is still released on the no-owner path", func(t *testing.T) {
		d := createVPCTestDaemon(t)
		id := fmt.Sprintf("i-alb-gone-%d", time.Now().UnixNano())
		allocID, _, svc := bindRealEIP(t, d, id, tags.ManagedByELBv2)

		_ = d.TerminateSystemInstance(id)
		assert.False(t, addressKept(t, svc, allocID), "an internet-facing ALB address is released when no node owns the VM")
	})
}
