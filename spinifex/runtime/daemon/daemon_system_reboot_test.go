package daemon

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/systeminstance"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/qmp"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withMonitor gives a test VM a monitor client. A reboot ends the old process
// and marks that client dead, which makes "Reboot ran" observable.
func withMonitor(v *vm.VM) *qmp.QMPClient {
	v.QMPClient = &qmp.QMPClient{}
	return v.QMPClient
}

func monitorDead(q *qmp.QMPClient) bool {
	q.Mu.Lock()
	defer q.Mu.Unlock()
	return q.Dead
}

func TestS7_LBRepairRefusesACustomerInstance(t *testing.T) {
	cases := []struct {
		name string
		inst *vm.VM
		loc  recordLocation
	}{
		{"running customer", &vm.VM{ID: "i-r-running", Status: vm.StateRunning, AccountID: testAccountID}, atLive},
		{"stopped customer", &vm.VM{ID: "i-r-stopped", Status: vm.StateStopped, AccountID: testAccountID}, atStopped},
		{"load balancer manager in a customer account", &vm.VM{ID: "i-r-lbcust", Status: vm.StateRunning, AccountID: testAccountID, ManagedBy: tags.ManagedByELBv2}, atLive},
		{"customer account holding an EKS tag", &vm.VM{ID: "i-r-eks-cust", Status: vm.StateRunning, AccountID: testAccountID, ManagedBy: tags.ManagedByEKS}, atLive},
		{"system VM that is not a load balancer", &vm.VM{ID: "i-r-eks", Status: vm.StateRunning, AccountID: awsidentifiers.GlobalAccountID, ManagedBy: tags.ManagedByEKS}, atLive},
	}
	for _, route := range []string{"local", "wrapper", "routed", "nats launcher"} {
		t.Run(route, func(t *testing.T) {
			d := guardDaemon(t)
			terminated := countTerminations(d)
			launcher := elbv2domain.NewNATSSystemInstanceLauncher(d.natsConn, 5*time.Second)

			for _, c := range cases {
				mon := withMonitor(c.inst)
				stage(t, d, c.inst, c.loc)
				var err error
				switch route {
				case "local":
					err = d.rebootSystemInstanceLocal(c.inst.ID)
				case "wrapper":
					err = d.RebootSystemInstance(c.inst.ID)
				case "routed":
					require.NoError(t, d.subscribeSystemReboot(c.inst.ID))
					err = d.rebootSystemInstanceRemote(c.inst.ID)
				case "nats launcher":
					require.NoError(t, d.subscribeSystemReboot(c.inst.ID))
					err = launcher.RebootSystemInstance(c.inst.ID)
				}
				require.Error(t, err, c.name)
				assert.Contains(t, err.Error(), systeminstance.ErrSystemInstanceNotSystem.Error(), c.name)
				assert.NotContains(t, err.Error(), systeminstance.ErrSystemInstanceNotFound.Error(), c.name)
				d.vmMgr.WaitForBackgroundWork()
				assert.False(t, monitorDead(mon), "%s: Reboot must never be reached", c.name)
			}
			assert.Zero(t, terminated.Load())
		})
	}
}

func TestS7_LBRepairRebootsAndNeverTerminates(t *testing.T) {
	for _, route := range []string{"wrapper", "routed", "nats launcher"} {
		t.Run(route, func(t *testing.T) {
			d := guardDaemon(t)
			var mu sync.Mutex
			var moves []vm.InstanceState
			d.vmMgr.SetDeps(vm.Deps{NodeID: d.node, TransitionState: func(_ *vm.VM, to vm.InstanceState) error {
				mu.Lock()
				defer mu.Unlock()
				moves = append(moves, to)
				return nil
			}})
			eip := &sysInstReclaimingEIP{}
			d.eipService = eip
			lb := &vm.VM{ID: "i-lb-" + strings.ReplaceAll(route, " ", "-"), Status: vm.StateRunning, AccountID: awsidentifiers.GlobalAccountID, ManagedBy: tags.ManagedByELBv2}
			mon := withMonitor(lb)
			stage(t, d, lb, atLive)

			var err error
			switch route {
			case "wrapper":
				err = d.RebootSystemInstance(lb.ID)
			case "routed":
				require.NoError(t, d.subscribeSystemReboot(lb.ID))
				err = d.rebootSystemInstanceRemote(lb.ID)
			case "nats launcher":
				require.NoError(t, d.subscribeSystemReboot(lb.ID))
				err = elbv2domain.NewNATSSystemInstanceLauncher(d.natsConn, 5*time.Second).RebootSystemInstance(lb.ID)
			}
			require.NoError(t, err)

			require.Eventually(t, func() bool { return monitorDead(mon) }, 5*time.Second, 10*time.Millisecond, "the owner restarts the guest")
			d.vmMgr.WaitForBackgroundWork()
			assert.NotEqual(t, vm.StateTerminated, d.vmMgr.Status(lb))
			mu.Lock()
			defer mu.Unlock()
			assert.NotContains(t, moves, vm.StateShuttingDown, "a repair never terminates")
			assert.NotContains(t, moves, vm.StateTerminated, "a repair never terminates")
			assert.Empty(t, eip.released, "a repair never releases an address")
			assert.Empty(t, eip.reclaimed)
		})
	}
}

func TestRebootSystemInstance_NoOwnerAndUnreadableRecord(t *testing.T) {
	t.Run("no owner subscribed", func(t *testing.T) {
		d := guardDaemon(t)
		err := d.rebootSystemInstanceRemote("i-nobody-runs-this")
		require.ErrorIs(t, err, systeminstance.ErrSystemInstanceNotFound)

		err = elbv2domain.NewNATSSystemInstanceLauncher(d.natsConn, 2*time.Second).RebootSystemInstance("i-nobody-runs-this")
		require.Error(t, err)
		assert.ErrorIs(t, err, nats.ErrNoResponders, "an unreachable owner stays distinguishable")
	})

	t.Run("a record this node cannot read", func(t *testing.T) {
		d := guardDaemon(t)
		lb := &vm.VM{ID: "i-lb-unreadable", Status: vm.StateRunning, AccountID: awsidentifiers.GlobalAccountID, ManagedBy: tags.ManagedByELBv2}
		mon := withMonitor(lb)
		d.vmMgr.Insert(lb)
		d.ownership = fakeOwnership{err: errInjected}

		err := d.RebootSystemInstance(lb.ID)
		require.ErrorIs(t, err, systeminstance.ErrSystemInstanceOwnershipUnknown)
		d.vmMgr.WaitForBackgroundWork()
		assert.False(t, monitorDead(mon))
	})

	t.Run("a VM that is not running", func(t *testing.T) {
		d := guardDaemon(t)
		lb := &vm.VM{ID: "i-lb-stopped", Status: vm.StateStopped, AccountID: awsidentifiers.GlobalAccountID, ManagedBy: tags.ManagedByELBv2}
		stage(t, d, lb, atLive)
		require.Error(t, d.RebootSystemInstance(lb.ID))
	})
}

func TestRebootSystemInstanceRemote_ReplyAndConnectionFailures(t *testing.T) {
	t.Run("no connection reads as an unreachable owner", func(t *testing.T) {
		d := guardDaemon(t)
		d.natsConn = nil
		require.ErrorIs(t, d.rebootSystemInstanceRemote("i-lb"), systeminstance.ErrSystemInstanceNotFound)
	})

	t.Run("an undecodable reply is an error, not a reboot", func(t *testing.T) {
		d := guardDaemon(t)
		sub, err := d.natsConn.Subscribe(systemRebootSubject("i-garbled"), func(msg *nats.Msg) {
			_ = msg.Respond([]byte("{not json"))
		})
		require.NoError(t, err)
		defer func() { _ = sub.Unsubscribe() }()

		err = d.rebootSystemInstanceRemote("i-garbled")
		require.Error(t, err)
		assert.NotErrorIs(t, err, systeminstance.ErrSystemInstanceNotFound)
	})

	t.Run("an owner that refuses is reported to the caller", func(t *testing.T) {
		d := guardDaemon(t)
		sub, err := d.natsConn.Subscribe(systemRebootSubject("i-refusing"), func(msg *nats.Msg) {
			respondWithSystemRebootReply(msg, "not a system instance")
		})
		require.NoError(t, err)
		defer func() { _ = sub.Unsubscribe() }()

		require.Error(t, d.rebootSystemInstanceRemote("i-refusing"))
	})
}

func TestHandleSystemRebootInstance_RejectsAMalformedSubject(t *testing.T) {
	d := guardDaemon(t)
	sub, err := d.natsConn.Subscribe("system.Elsewhere.i-lb", d.handleSystemRebootInstance)
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	reply, err := d.natsConn.Request("system.Elsewhere.i-lb", nil, 2*time.Second)
	require.NoError(t, err)
	assert.Contains(t, string(reply.Data), "InvalidInstanceID.NotFound")
	d.systemDispatchWg.Wait()
}

func TestSubscribeSystemReboot_IdempotentAndSkippedWithoutConnection(t *testing.T) {
	d := guardDaemon(t)
	require.NoError(t, d.subscribeSystemReboot("i-lb"))
	require.NoError(t, d.subscribeSystemReboot("i-lb"))
	assert.Contains(t, d.natsSubscriptions, systemRebootSubject("i-lb"))

	d.natsConn = nil
	require.NoError(t, d.subscribeSystemReboot("i-other"))
	assert.NotContains(t, d.natsSubscriptions, systemRebootSubject("i-other"))
}
