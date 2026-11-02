package cluster

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/eks"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// holdLaunch blocks the first control-plane launch until the returned release
// is called, and closes held once the launch has reached it.
func holdLaunch(t *testing.T, inst *fakeK3sInst) (held <-chan struct{}, release func()) {
	t.Helper()
	h := make(chan struct{})
	gate := make(chan struct{})
	var once, releaseOnce sync.Once
	inst.launchHook = func() {
		first := false
		once.Do(func() { first = true })
		if first {
			close(h)
			<-gate
		}
	}
	release = func() { releaseOnce.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return h, release
}

// silenceLaunches models a launch whose node stopped heartbeating, so the
// teardown no longer waits for it and can sweep the record under it.
func silenceLaunches(svc *EKSServiceImpl) {
	svc.launchHeartbeatEvery = 0
	svc.launchStaleAfter = time.Nanosecond
}

func eventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	require.Eventually(t, cond, 10*time.Second, 10*time.Millisecond, msg)
}

// TestS2_EKSOvertakenLaunchNeverWritesANewerClusterOfTheSameName: a launch
// whose cluster was deleted and swept, and whose name a tenant then created
// again, must not overwrite the newer record or delete anything by name. It
// releases only the instance it launched itself.
func TestS2_EKSOvertakenLaunchNeverWritesANewerClusterOfTheSameName(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	silenceLaunches(f.svc)
	f.inst.uniqueIDs = true
	held, release := holdLaunch(t, f.inst)

	_, err := f.svc.CreateCluster(context.Background(), createInput("alpha"), testAccountID, "")
	require.NoError(t, err)
	<-held
	_, err = f.svc.DeleteCluster(context.Background(), deleteInput("alpha"), testAccountID)
	require.NoError(t, err)
	f.svc.WaitTeardowns()
	_, err = GetClusterMeta(t.Context(), f.kv, "alpha")
	require.ErrorIs(t, err, ErrClusterNotFound)

	_, err = f.svc.CreateCluster(context.Background(), createInput("alpha"), testAccountID, "")
	require.NoError(t, err, "the name is free once the first cluster's record is gone")
	var newer *ClusterMeta
	eventually(t, func() bool {
		newer, err = GetClusterMeta(t.Context(), f.kv, "alpha")
		return err == nil && newer.ControlPlaneInstanceID != ""
	}, "the newer cluster's launch records its control plane")
	deletesBefore := len(f.nlb.deleteLBCalls)

	release()
	f.svc.WaitLaunches()
	f.svc.WaitTeardowns()

	after, err := GetClusterMeta(t.Context(), f.kv, "alpha")
	require.NoError(t, err)
	assert.True(t, after.CreatedAt.Equal(newer.CreatedAt), "the newer cluster's record must not be overwritten by the older launch")
	assert.Equal(t, newer.ControlPlaneInstanceID, after.ControlPlaneInstanceID)
	got := f.inst.terminated()
	assert.NotContains(t, got, newer.ControlPlaneInstanceID, "the newer cluster's control plane is not the older launch's to release")
	require.Len(t, got, 1, "the older launch releases the one instance it started: %v", got)
	assert.Len(t, f.nlb.deleteLBCalls, deletesBefore, "nothing is deleted by name on the newer cluster's behalf")
}

// TestS2_EKSOvertakenLaunchHandsItsInfraToTheTeardown: a launch that finds its
// record already swept records what it built as a DELETING cluster, so a
// failed release is retried by the teardown and its reaper, not dropped.
func TestS2_EKSOvertakenLaunchHandsItsInfraToTheTeardown(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	silenceLaunches(f.svc)
	held, release := holdLaunch(t, f.inst)

	_, err := f.svc.CreateCluster(context.Background(), createInput("alpha"), testAccountID, "")
	require.NoError(t, err)
	<-held
	_, err = f.svc.DeleteCluster(context.Background(), deleteInput("alpha"), testAccountID)
	require.NoError(t, err)
	f.svc.WaitTeardowns()
	f.inst.mu.Lock()
	f.inst.terminateErr = errors.New("hypervisor unreachable")
	f.inst.mu.Unlock()
	release()
	f.svc.WaitLaunches()
	f.svc.WaitTeardowns()

	meta, err := GetClusterMeta(t.Context(), f.kv, "alpha")
	require.NoError(t, err, "the launch's control plane must stay owned by a record when its release fails")
	assert.Equal(t, ClusterStatusDeleting, meta.Status)
	assert.Equal(t, "i-aaa111", meta.ControlPlaneInstanceID)

	f.inst.mu.Lock()
	f.inst.terminateErr = nil
	f.inst.mu.Unlock()
	redrive(t, f.svc)
	_, err = GetClusterMeta(t.Context(), f.kv, "alpha")
	require.ErrorIs(t, err, ErrClusterNotFound, "the reaper finishes the release")
}

// TestS2_EKSLaunchMarkedFailedReleasesWhatItBuilt: a record marked FAILED
// under a running launch (the create timed out) is not a teardown owning the
// launch's refs, so the launch merges them in and releases what it built.
func TestS2_EKSLaunchMarkedFailedReleasesWhatItBuilt(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	held, release := holdLaunch(t, f.inst)

	_, err := f.svc.CreateCluster(context.Background(), createInput("alpha"), testAccountID, "")
	require.NoError(t, err)
	<-held
	require.NoError(t, MarkClusterFailed(t.Context(), f.kv, "alpha", "create timed out"))
	release()
	f.svc.WaitLaunches()

	assert.Contains(t, f.inst.terminated(), "i-aaa111", "the control plane launched after FAILED must be released")
	meta, err := GetClusterMeta(t.Context(), f.kv, "alpha")
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusFailed, meta.Status)
}

// TestS2_EKSFreshClaimInheritsNoStaleChildRecords: keys an earlier cluster of
// the same name left behind are not inherited by a new one.
func TestS2_EKSFreshClaimInheritsNoStaleChildRecords(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	stale := []string{AdminKubeconfigKey("alpha"), NodeTokenKey("alpha"), "clusters/alpha/access-entries/old"}
	for _, k := range stale {
		_, err := f.kv.Put(t.Context(), k, []byte("stale"))
		require.NoError(t, err)
	}
	f.inst.launchErr = errors.New("no capacity") // stop the launch early; only the claim matters

	_, err := f.svc.CreateCluster(context.Background(), createInput("alpha"), testAccountID, "")
	require.NoError(t, err)
	f.svc.WaitLaunches()

	for _, k := range stale {
		entry, err := f.kv.Get(t.Context(), k)
		if err == nil {
			assert.NotEqual(t, "stale", string(entry.Value()), "%s must not be inherited from an earlier cluster", k)
		}
	}
}

// TestS2_EKSBootstrapWritesNothingIntoADeletingCluster: a bootstrap subscriber
// still running for a cluster being deleted must not write its artifacts.
func TestS2_EKSBootstrapWritesNothingIntoADeletingCluster(t *testing.T) {
	t.Parallel()
	f := newDeleteClusterFixture(t, "alpha")
	_, _, err := MarkClusterDeleting(t.Context(), f.kv, "alpha")
	require.NoError(t, err)
	boot, err := NewNATSBootstrap(f.svc.deps.NATSConn, f.kv, bootstrapTestMasterKey, testAccountID, "alpha")
	require.NoError(t, err)

	err = boot.persistToken(t.Context(), []byte(`{"token":"K10abc"}`))
	require.ErrorIs(t, err, errBootstrapPermanent)
	_, getErr := f.kv.Get(t.Context(), NodeTokenKey("alpha"))
	require.Error(t, getErr, "no node token is written into a DELETING cluster")
}

// TestS2_EKSLastNodegroupDeletedResumesTheClusterDelete: a cluster delete
// waiting on node groups resumes when the last one is deleted, and the wait
// itself never counted as a failed pass.
func TestS2_EKSLastNodegroupDeletedResumesTheClusterDelete(t *testing.T) {
	t.Parallel()
	f := newDeleteClusterFixture(t, "alpha")
	markDeleting(t, f, "alpha", 10*time.Minute)
	require.NoError(t, PutNodegroupRecord(t.Context(), f.kv, &NodegroupRecord{
		ClusterName: "alpha", Name: "ng1", Status: eks.NodegroupStatusActive,
	}))
	redrive(t, f.svc)
	meta, err := GetClusterMeta(t.Context(), f.kv, "alpha")
	require.NoError(t, err)
	assert.Zero(t, meta.DeleteReapAttempts, "waiting on node groups must not grow the backoff")

	_, err = f.svc.DeleteNodegroup(context.Background(), &eks.DeleteNodegroupInput{
		ClusterName: aws.String("alpha"), NodegroupName: aws.String("ng1"),
	}, testAccountID)
	require.NoError(t, err)
	f.svc.WaitTeardowns()

	_, err = GetClusterMeta(t.Context(), f.kv, "alpha")
	assert.ErrorIs(t, err, ErrClusterNotFound, "deleting the last node group lets the cluster delete finish")
}

// TestS8_EKSHungTeardownStepReleasesTheGate: a pass is bounded, so a step that
// never returns cannot hold the gate and the lease for ever; the pass is
// recorded as failed and the cluster can be handed out again.
func TestS8_EKSHungTeardownStepReleasesTheGate(t *testing.T) {
	f := newDeleteClusterFixture(t, "alpha")
	orig := teardownPassTimeout
	teardownPassTimeout = 300 * time.Millisecond
	t.Cleanup(func() { teardownPassTimeout = orig })
	var once sync.Once
	f.nlb.describeLBHook = func(ctx context.Context) { once.Do(func() { <-ctx.Done() }) }

	_, err := f.svc.DeleteCluster(context.Background(), deleteInput("alpha"), testAccountID)
	require.NoError(t, err)
	done := make(chan struct{})
	go func() { f.svc.WaitTeardowns(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a hung teardown step must not hold the teardown past its pass deadline")
	}

	meta, err := GetClusterMeta(t.Context(), f.kv, "alpha")
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusDeleting, meta.Status)
	assert.NotEmpty(t, meta.LastDeleteReapError, "the timed-out pass is recorded so the reaper backs off")
	assert.True(t, f.svc.startTeardown(testAccountID, "alpha"), "the gate and lease are free again")
	f.svc.WaitTeardowns()
}

// TestS2_EKSFailedClusterReclaimRunsUnderTheTeardownLease: re-creating a FAILED
// cluster's name tears the old attempt down, so it is refused while another
// node holds that cluster's teardown.
func TestS2_EKSFailedClusterReclaimRunsUnderTheTeardownLease(t *testing.T) {
	t.Parallel()
	f := newDeleteClusterFixture(t, "alpha")
	meta, err := GetClusterMeta(t.Context(), f.kv, "alpha")
	require.NoError(t, err)
	meta.Status = ClusterStatusFailed
	require.NoError(t, PutClusterMeta(t.Context(), f.kv, meta))
	_, err = f.svc.leaderKV.Create(t.Context(), teardownLeaderKey(testAccountID, "alpha"), []byte("node-2"))
	require.NoError(t, err)

	_, err = f.svc.CreateCluster(context.Background(), createInput("alpha"), testAccountID, "")
	code, _, _ := awserrors.ResolveErrorDetail(err)
	assert.Equal(t, awserrors.ErrorEKSResourceInUse, code)
	f.svc.WaitLaunches()
	assert.Empty(t, f.inst.terminated(), "no teardown of the old attempt while another node holds it")
}

// csiVolumeRecorder lists one CSI volume owned by the cluster and records any
// delete, which the cluster teardown must never issue.
type csiVolumeRecorder struct {
	mu      sync.Mutex
	deleted []string
}

func (c *csiVolumeRecorder) DescribeVolumes(context.Context, *ec2.DescribeVolumesInput, string) (*ec2.DescribeVolumesOutput, error) {
	return &ec2.DescribeVolumesOutput{Volumes: []*ec2.Volume{
		tagVol("vol-pvc", map[string]string{"kubernetes.io/cluster/alpha": "owned"}),
	}}, nil
}

func (c *csiVolumeRecorder) DeleteVolume(_ context.Context, in *ec2.DeleteVolumeInput, _ string) (*ec2.DeleteVolumeOutput, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleted = append(c.deleted, aws.StringValue(in.VolumeId))
	return &ec2.DeleteVolumeOutput{}, nil
}

// TestS7_EKSTeardownNeverDeletesCSIVolumesOrWorkers: the cluster teardown
// removes the control plane the tenant asked to delete and nothing holding the
// tenant's data: no CSI-provisioned volume and no node group worker.
func TestS7_EKSTeardownNeverDeletesCSIVolumesOrWorkers(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	vols := &csiVolumeRecorder{}
	f.svc.deps.Volume = vols
	meta := sampleClusterMeta("alpha")
	meta.ControlPlaneInstanceID = "i-aaa111"
	require.NoError(t, PutClusterMeta(t.Context(), f.kv, meta))

	deleteAndSettle(t, f.svc, "alpha")

	_, err := GetClusterMeta(t.Context(), f.kv, "alpha")
	require.ErrorIs(t, err, ErrClusterNotFound)
	vols.mu.Lock()
	assert.Empty(t, vols.deleted, "ADR-0003:S7: a CSI volume holds tenant data and is only ever reported")
	vols.mu.Unlock()
	f.worker.mu.Lock()
	assert.Empty(t, f.worker.terminateCalls, "ADR-0003:S7: workers are terminated only by DeleteNodegroup")
	f.worker.mu.Unlock()
}
