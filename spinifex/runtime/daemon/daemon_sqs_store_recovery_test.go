//test:in-package — recoverable, runsOn and the recovery pass are unexported.

package daemon

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A survivor must not claim, launch or abandon an SQS store whose node went
// stale, while an ordinary instance on the same node is still attempted.
func TestSQSStoreIsNeverRecoveredOntoAnotherNode(t *testing.T) {
	s := newRecoveryStore(t)
	d, r := recoveryDaemon(t, s)
	var relaunched []string
	d.vmMgr.SetDeps(vm.Deps{
		NodeID: d.node,
		Hooks: vm.ManagerHooks{BeforeInstanceRelaunch: func(v *vm.VM) error {
			relaunched = append(relaunched, v.ID)
			return nil
		}},
	})
	s.beat(t, "node-dead", time.Now().Add(-time.Hour))

	store := runningOn("i-sqs-store", "node-dead")
	store.Spec.InstanceType = schedulableType(t, d)
	store.Spec.ManagedBy = tags.ManagedBySQS
	store.Spec.SQSStoreGuest = &vm.SQSStoreGuest{NodeID: "node-dead", DataVolumeID: "vol-0123456789abcdef0"}
	store.Status.Instance = &ec2.Instance{InstanceId: aws.String("i-sqs-store")}
	require.NoError(t, s.m.records.Set(t.Context(), instanceRecordKey("i-sqs-store"), store))
	s.put(t, "i-ordinary", "node-dead", "zz9.none")

	r.pass(t.Context())
	r.pass(t.Context())

	got := s.load(t, "i-sqs-store")
	assert.Equal(t, "node-dead", got.Status.LastNode, "the store must not be claimed")
	assert.Equal(t, vm.DesiredRunning, got.Spec.DesiredState, "nor given up on and stopped")
	assert.Equal(t, vm.StateRunning, got.Status.Status)
	assert.NotContains(t, r.backoff, "i-sqs-store", "no attempt may be made, so none is budgeted")
	assert.NotContains(t, relaunched, "i-sqs-store")
	assert.False(t, d.vmMgr.UpdateState("i-sqs-store", func(*vm.VM) {}), "nothing is launched here")

	assert.Equal(t, 1, r.backoff["i-ordinary"].attempts, "the pass still attempts ordinary instances")

	_, err := s.m.ClaimRecoverableInstance("i-sqs-store", "node-dead", d.node)
	require.ErrorIs(t, err, vm.ErrRecoveryClaimLost, "the claim itself refuses, not only candidate selection")
	assert.Equal(t, "node-dead", s.load(t, "i-sqs-store").Status.LastNode)
}

// Excluding it from cross-node recovery must not drop it from its own node's
// restore set.
func TestSQSStoreStillRestoresOnItsOwnNode(t *testing.T) {
	store := runningOn("i-sqs-store", "node-1")
	store.Spec.ManagedBy = tags.ManagedBySQS
	assert.True(t, runsOn(store, "node-1"))
	assert.False(t, runsOn(store, "node-2"))
}
