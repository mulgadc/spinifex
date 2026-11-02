package cluster

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestS2_EKSDeletedClusterNeverReadsMissingWhileItsLaunchRuns: while a create
// launch for the cluster is still running, the teardown keeps the record, so
// DescribeCluster reads DELETING throughout and never NotFound followed by
// DELETING again. The launch hands its refs over and the delete then finishes.
func TestS2_EKSDeletedClusterNeverReadsMissingWhileItsLaunchRuns(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	f.svc.launchHeartbeatEvery = 20 * time.Millisecond
	f.svc.launchStaleAfter = 5 * time.Second
	held, release := holdLaunch(t, f.inst)

	_, err := f.svc.CreateCluster(context.Background(), createInput("alpha"), testAccountID, "")
	require.NoError(t, err)
	<-held
	_, err = f.svc.DeleteCluster(context.Background(), deleteInput("alpha"), testAccountID)
	require.NoError(t, err)
	f.svc.WaitTeardowns()

	for range 5 {
		out, err := f.svc.DescribeCluster(context.Background(), &eks.DescribeClusterInput{Name: aws.String("alpha")}, testAccountID)
		require.NoError(t, err, "the record must stay while its launch is still running")
		assert.Equal(t, eks.ClusterStatusDeleting, aws.StringValue(out.Cluster.Status))
		time.Sleep(30 * time.Millisecond)
	}
	meta, err := GetClusterMeta(t.Context(), f.kv, "alpha")
	require.NoError(t, err)
	assert.Equal(t, stepLaunch, meta.DeleteFailedStep)
	assert.Zero(t, meta.DeleteReapAttempts, "waiting for the launch is not a failed pass")

	release()
	f.svc.WaitLaunches()
	f.svc.WaitTeardowns()
	_, err = GetClusterMeta(t.Context(), f.kv, "alpha")
	require.ErrorIs(t, err, ErrClusterNotFound, "once the launch hands over, the delete finishes")
	assert.Contains(t, f.inst.terminated(), "i-aaa111", "the VM the launch started is torn down with the cluster")
}

// TestS2_EKSCrashedLaunchDoesNotPinTheDelete: a launch owner whose heartbeat
// stopped is taken as crashed once it is stale, and the delete proceeds.
func TestS2_EKSCrashedLaunchDoesNotPinTheDelete(t *testing.T) {
	t.Parallel()
	f := newDeleteClusterFixture(t, "alpha")
	markDeleting(t, f, "alpha", 10*time.Minute)
	require.NoError(t, casUpdateMeta(t.Context(), f.kv, "alpha", func(m *ClusterMeta) bool {
		m.LaunchOwner = "node-gone"
		m.LaunchHeartbeat = time.Now().UTC().Add(-10 * time.Minute)
		return true
	}))

	redrive(t, f.svc)

	_, err := GetClusterMeta(t.Context(), f.kv, "alpha")
	assert.ErrorIs(t, err, ErrClusterNotFound, "a stale launch owner must not hold the delete for ever")
}

// TestS2_EKSFailedClusterReclaimIsNotRefusedForCapacity: the per-node bound
// on teardowns is about background work; re-creating a FAILED cluster's name
// must not be answered ResourceInUse because the node is busy elsewhere.
func TestS2_EKSFailedClusterReclaimIsNotRefusedForCapacity(t *testing.T) {
	f := newDeleteClusterFixture(t, "alpha")
	meta, err := GetClusterMeta(t.Context(), f.kv, "alpha")
	require.NoError(t, err)
	meta.Status = ClusterStatusFailed
	require.NoError(t, PutClusterMeta(t.Context(), f.kv, meta))
	orig := teardownConcurrency
	teardownConcurrency = 0
	t.Cleanup(func() { teardownConcurrency = orig })

	_, err = f.svc.CreateCluster(context.Background(), createInput("alpha"), testAccountID, "")
	require.NoError(t, err, "a full teardown queue is not the name being in use")
	f.svc.WaitLaunches()
}

// TestS2_EKSOvertakenLaunchCannotReplaceANewerClustersSigningKey: an old
// launch that resumes after its cluster was deleted and the name re-created
// must not overwrite the newer cluster's OIDC signing key, which would break
// every IRSA token the newer cluster issues.
func TestS2_EKSOvertakenLaunchCannotReplaceANewerClustersSigningKey(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	silenceLaunches(f.svc)
	held := make(chan struct{})
	gate := make(chan struct{})
	var first atomic.Bool
	f.svc.launchStepHook = func(step string) {
		if step == "oidc-keypair" && first.CompareAndSwap(false, true) {
			close(held)
			<-gate
		}
	}
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	})

	_, err := f.svc.CreateCluster(context.Background(), createInput("alpha"), testAccountID, "")
	require.NoError(t, err)
	<-held
	_, err = f.svc.DeleteCluster(context.Background(), deleteInput("alpha"), testAccountID)
	require.NoError(t, err)
	f.svc.WaitTeardowns()
	_, err = f.svc.CreateCluster(context.Background(), createInput("alpha"), testAccountID, "")
	require.NoError(t, err)
	eventually(t, func() bool {
		m, err := GetClusterMeta(t.Context(), f.kv, "alpha")
		return err == nil && m.ControlPlaneInstanceID != ""
	}, "the newer cluster's launch gets past its keypair")
	newer, err := f.kv.Get(t.Context(), OIDCSigningKeyKey("alpha"))
	require.NoError(t, err)

	close(gate)
	f.svc.WaitLaunches()
	f.svc.WaitTeardowns()

	after, err := f.kv.Get(t.Context(), OIDCSigningKeyKey("alpha"))
	require.NoError(t, err)
	assert.Equal(t, string(newer.Value()), string(after.Value()), "the newer cluster's signing key must survive the older launch")
}

// blockingDeleteKV holds a delete of one key until its context ends.
type blockingDeleteKV struct {
	jetstream.KeyValue

	block string
}

func (b *blockingDeleteKV) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	if key == b.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return b.KeyValue.Delete(ctx, key, opts...)
}

// TestS8_EKSDeadlineDuringRecordRemovalIsRecorded: a pass that runs out of time
// while removing the record is recorded like any other failed pass.
func TestS8_EKSDeadlineDuringRecordRemovalIsRecorded(t *testing.T) {
	f := newDeleteClusterFixture(t, "alpha")
	orig := teardownPassTimeout
	teardownPassTimeout = 300 * time.Millisecond
	t.Cleanup(func() { teardownPassTimeout = orig })
	_, err := f.kv.Put(t.Context(), "clusters/alpha/addons/vpc-cni", []byte(`{}`))
	require.NoError(t, err)
	f.svc.teardownKV = func(context.Context, string) (jetstream.KeyValue, error) {
		return &blockingDeleteKV{KeyValue: f.kv, block: "clusters/alpha/addons/vpc-cni"}, nil
	}

	deleteAndSettle(t, f.svc, "alpha")

	meta := requireTeardownRetained(t, f.kv, "alpha")
	assert.Equal(t, stepRecord, meta.DeleteFailedStep)
}

// TestEKSBootstrapRunRemovesItsOwnEntryOnExit: a bootstrap subscriber that
// returns on its own leaves no entry behind for stopBootstrap to find.
func TestEKSBootstrapRunRemovesItsOwnEntryOnExit(t *testing.T) {
	t.Parallel()
	f := newDeleteClusterFixture(t, "alpha")

	f.svc.spawnBootstrap(testAccountID, "alpha", f.kv, []string{"no-such-kind"})

	eventually(t, func() bool {
		f.svc.bootstrapMu.Lock()
		defer f.svc.bootstrapMu.Unlock()
		_, ok := f.svc.bootstraps[reconcilerLeaderKey(testAccountID, "alpha")]
		return !ok
	}, "an exited bootstrap must remove its own entry")
}

// TestS7_EKSTeardownRefusesANonDeletingRecordEvenWithNodegroups: the S7
// boundary is checked before anything else, so an ACTIVE record with node
// groups is refused as not recorded, never reported as waiting on them.
func TestS7_EKSTeardownRefusesANonDeletingRecordEvenWithNodegroups(t *testing.T) {
	t.Parallel()
	f := newDeleteClusterFixture(t, "alpha")
	meta, err := GetClusterMeta(t.Context(), f.kv, "alpha")
	require.NoError(t, err)
	meta.Status = ClusterStatusActive
	require.NoError(t, PutClusterMeta(t.Context(), f.kv, meta))
	require.NoError(t, PutNodegroupRecord(t.Context(), f.kv, &NodegroupRecord{
		ClusterName: "alpha", Name: "ng1", Status: eks.NodegroupStatusActive,
	}))

	err = f.svc.convergeClusterDeletion(t.Context(), testAccountID, "alpha", nil)
	require.ErrorIs(t, err, errTeardownNotRecorded)
	require.NotErrorIs(t, err, errNodegroupsAttached)
	after, err := GetClusterMeta(t.Context(), f.kv, "alpha")
	require.NoError(t, err)
	assert.Empty(t, after.DeleteFailedStep, "nothing is recorded on a cluster nobody asked to delete")
}
