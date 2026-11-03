package daemon

import (
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/systeminstance"
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
