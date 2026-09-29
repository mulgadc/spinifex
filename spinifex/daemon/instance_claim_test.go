package daemon_test

import (
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/daemon"
	"github.com/mulgadc/spinifex/spinifex/foundation/lifecycle/resource"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recoverableRecord is what a node that died hard leaves behind: observed
// running, still wanted running, and named to a node that will never write
// again.
func recoverableRecord(id, node string) *vm.InstanceRecord {
	return &vm.InstanceRecord{
		Metadata: resource.Metadata{Name: id, AccountID: "111122223333"},
		Spec:     vm.InstanceSpec{InstanceType: "t3.micro", DesiredState: vm.DesiredRunning},
		Status:   vm.InstanceStatus{Status: vm.StateRunning, LastNode: node},
	}
}

func TestClaimRecoverableInstance_TakesOwnership(t *testing.T) {
	m := newRecordManager(t)
	require.NoError(t, m.WriteInstanceRecord("i-1", recoverableRecord("i-1", "node-1")))

	claimed, err := m.ClaimRecoverableInstance("i-1", "node-1", "node-2")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.Equal(t, "i-1", claimed.ID)

	stored, err := m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	assert.Equal(t, "node-2", stored.Status.LastNode,
		"the claim is the ownership transfer, so it has to be durable before the launch starts")
}

// The CAS is the whole of the mutual exclusion. Several survivors derive the
// same candidate and race it; exactly one revision may win.
func TestClaimRecoverableInstance_OnlyOneSurvivorWins(t *testing.T) {
	m := newRecordManager(t)
	require.NoError(t, m.WriteInstanceRecord("i-1", recoverableRecord("i-1", "node-1")))

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
	)
	for _, claimant := range []string{"node-2", "node-3", "node-4"} {
		wg.Go(func() {
			if _, err := m.ClaimRecoverableInstance("i-1", "node-1", claimant); err == nil {
				mu.Lock()
				winners = append(winners, claimant)
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	require.Lenf(t, winners, 1, "more than one node claimed the same instance: %v", winners)

	stored, err := m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	assert.Equal(t, winners[0], stored.Status.LastNode)
}

// A loser reads a record naming a live owner, which is how the candidate
// disappears from its view without anything telling it so.
func TestClaimRecoverableInstance_LostWhenTheOwnerAlreadyMoved(t *testing.T) {
	m := newRecordManager(t)
	require.NoError(t, m.WriteInstanceRecord("i-1", recoverableRecord("i-1", "node-3")))

	_, err := m.ClaimRecoverableInstance("i-1", "node-1", "node-2")
	assert.ErrorIs(t, err, vm.ErrRecoveryClaimLost)
}

func TestClaimRecoverableInstance_LostWhenTheRecordIsGone(t *testing.T) {
	m := newRecordManager(t)

	_, err := m.ClaimRecoverableInstance("i-never-written", "node-1", "node-2")
	assert.ErrorIs(t, err, vm.ErrRecoveryClaimLost)
}

// Ownership is checked and eligibility is checked again, at the point the
// transfer happens rather than only when the candidate was derived. A terminate
// that started in between must not be undone by a claim.
func TestClaimRecoverableInstance_RefusesAnInstanceNobodyWantsRunning(t *testing.T) {
	m := newRecordManager(t)

	stopped := recoverableRecord("i-stopped", "node-1")
	stopped.Spec.DesiredState = vm.DesiredStopped
	stopped.Status.Status = vm.StateStopped
	require.NoError(t, m.WriteInstanceRecord("i-stopped", stopped))

	terminating := recoverableRecord("i-terminating", "node-1")
	terminating.Status.Status = vm.StateShuttingDown
	require.NoError(t, m.WriteInstanceRecord("i-terminating", terminating))

	paused := recoverableRecord("i-paused", "node-1")
	paused.Status.Health.IOErrorSince = time.Now()
	require.NoError(t, m.WriteInstanceRecord("i-paused", paused))

	for _, id := range []string{"i-stopped", "i-terminating", "i-paused"} {
		_, err := m.ClaimRecoverableInstance(id, "node-1", "node-2")
		assert.ErrorIsf(t, err, vm.ErrRecoveryClaimLost, "%s should not be claimable", id)
	}
}

// Without the release a failed attempt parks the instance on a node that has
// just proved it cannot run it, and no other survivor looks at it again.
func TestReleaseRecoveredInstance_HandsTheRecordBack(t *testing.T) {
	m := newRecordManager(t)
	require.NoError(t, m.WriteInstanceRecord("i-1", recoverableRecord("i-1", "node-1")))

	_, err := m.ClaimRecoverableInstance("i-1", "node-1", "node-2")
	require.NoError(t, err)

	require.NoError(t, m.ReleaseRecoveredInstance("i-1", "node-2", "node-1"))

	stored, err := m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	assert.Equal(t, "node-1", stored.Status.LastNode)
}

// A release that arrives after someone else has taken the record must not pull
// it back: the current owner may already be running the guest.
func TestReleaseRecoveredInstance_LeavesAnotherNodesRecordAlone(t *testing.T) {
	m := newRecordManager(t)
	require.NoError(t, m.WriteInstanceRecord("i-1", recoverableRecord("i-1", "node-3")))

	require.NoError(t, m.ReleaseRecoveredInstance("i-1", "node-2", "node-1"))

	stored, err := m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	assert.Equal(t, "node-3", stored.Status.LastNode)
}

// The release is best-effort by design — the node making it may itself be
// failing — so an instance that has been terminated meanwhile is not an error.
func TestReleaseRecoveredInstance_AbsentRecordIsNotAnError(t *testing.T) {
	m := newRecordManager(t)

	assert.NoError(t, m.ReleaseRecoveredInstance("i-never-written", "node-2", "node-1"))
}

// Recovery is never inferred from a manager that has no record bucket: the
// answer would be "no instances to recover" and look identical to a healthy
// cluster.
func TestClaimAndRelease_RefuseWithoutABucket(t *testing.T) {
	m := &daemon.JetStreamManager{}

	_, err := m.ClaimRecoverableInstance("i-1", "node-1", "node-2")
	assert.Error(t, err)
	assert.NotErrorIs(t, err, vm.ErrRecoveryClaimLost,
		"an uninitialised bucket is a fault here, not an ordinary lost race")

	assert.Error(t, m.ReleaseRecoveredInstance("i-1", "node-2", "node-1"))
}

// AbandonRecovery is what stops an instance being retried forever, and the
// shape it leaves behind is the whole of its contract: the retrying ends on
// every node because they all derive their work from desired state, and the
// customer's own StartInstances is the retry.
func TestAbandonRecovery_LeavesTheInstanceStoppedWithAReason(t *testing.T) {
	m := newRecordManager(t)
	record := recoverableRecord("i-1", "node-1")
	record.Status.Instance = &ec2.Instance{InstanceId: aws.String("i-1")}
	require.NoError(t, m.WriteInstanceRecord("i-1", record))

	abandoned, err := m.AbandonRecovery("i-1", "node-1", "Server.HostRecoveryFailed", "nbdkit exited 1")
	require.NoError(t, err)
	require.True(t, abandoned)

	stored, err := m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	assert.Equal(t, vm.DesiredStopped, stored.Spec.DesiredState,
		"desired state is what every node's selection reads, so this is what ends the retrying cluster-wide")
	assert.Equal(t, vm.StateStopped, stored.Status.Status,
		"AWS leaves a host-failed instance stopped, not running on a node that is gone")
	require.NotNil(t, stored.Status.Instance.StateReason)
	assert.Equal(t, "Server.HostRecoveryFailed", aws.StringValue(stored.Status.Instance.StateReason.Code))
	assert.Equal(t, "nbdkit exited 1", aws.StringValue(stored.Status.Instance.StateReason.Message),
		"the reason is the only account of this the customer ever gets")
	assert.Equal(t, "node-1", stored.Status.LastNode,
		"where it last ran is still true and is the only pointer to the evidence")
}

// The give-up races the success. A node that exhausts its attempts after another
// survivor has already claimed and launched the instance must not stop a guest
// that is now running.
func TestAbandonRecovery_WillNotStopAnInstanceAnotherNodeTook(t *testing.T) {
	m := newRecordManager(t)
	require.NoError(t, m.WriteInstanceRecord("i-1", recoverableRecord("i-1", "node-3")))

	abandoned, err := m.AbandonRecovery("i-1", "node-1", "Server.HostRecoveryFailed", "gave up")
	require.NoError(t, err)
	assert.False(t, abandoned, "the caller has to be able to tell it did not apply")

	stored, err := m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	assert.Equal(t, vm.DesiredRunning, stored.Spec.DesiredState)
	assert.Equal(t, vm.StateRunning, stored.Status.Status)
}

// A record with no EC2 projection yet still has to stop being retried. The
// reason is lost, which is a worse outcome than having one and a better one than
// retrying forever.
func TestAbandonRecovery_WorksWithoutAnEC2Projection(t *testing.T) {
	m := newRecordManager(t)
	require.NoError(t, m.WriteInstanceRecord("i-1", recoverableRecord("i-1", "node-1")))

	abandoned, err := m.AbandonRecovery("i-1", "node-1", "Server.HostRecoveryFailed", "gave up")
	require.NoError(t, err)
	assert.True(t, abandoned)

	stored, err := m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	assert.Equal(t, vm.DesiredStopped, stored.Spec.DesiredState)
}

func TestAbandonRecovery_AbsentRecordIsNotAnError(t *testing.T) {
	m := newRecordManager(t)

	abandoned, err := m.AbandonRecovery("i-never-written", "node-1", "Server.HostRecoveryFailed", "gave up")
	assert.NoError(t, err)
	assert.False(t, abandoned)
}

func TestAbandonRecovery_RefusesWithoutABucket(t *testing.T) {
	m := &daemon.JetStreamManager{}

	_, err := m.AbandonRecovery("i-1", "node-1", "Server.HostRecoveryFailed", "gave up")
	assert.Error(t, err)
}
