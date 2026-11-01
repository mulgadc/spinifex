package cluster

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockTerminate holds the first control-plane terminate until release is
// called, and reports on entered once the teardown has reached it.
func blockTerminate(t *testing.T, inst *fakeK3sInst) (entered <-chan string, release func()) {
	t.Helper()
	in := make(chan string, 1)
	gate := make(chan struct{})
	var once, releaseOnce sync.Once
	inst.terminateHook = func(id string) {
		first := false
		once.Do(func() { first = true })
		if !first {
			return
		}
		in <- id
		<-gate
	}
	release = func() { releaseOnce.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return in, release
}

// seedHAControlPlane records two control-plane members on the fixture cluster.
func seedHAControlPlane(t *testing.T, f *deleteClusterFixture, name string) {
	t.Helper()
	meta, err := GetClusterMeta(t.Context(), f.kv, name)
	require.NoError(t, err)
	meta.ControlPlaneNodes = []ControlPlaneNode{
		{InstanceID: "i-aaa111", ENIID: "eni-aaa111", ENIIP: "10.0.1.42"},
		{InstanceID: "i-bbb222", ENIID: "eni-bbb222", ENIIP: "10.0.1.43"},
	}
	require.NoError(t, PutClusterMeta(t.Context(), f.kv, meta))
}

func waitEntered(t *testing.T, entered <-chan string) string {
	t.Helper()
	select {
	case id := <-entered:
		return id
	case <-time.After(10 * time.Second):
		t.Fatal("the teardown never reached the control-plane terminate")
		return ""
	}
}

func countOf(ids []string, id string) int {
	n := 0
	for _, v := range ids {
		if v == id {
			n++
		}
	}
	return n
}

// TestS8_EKSDeleteClusterReturnsBeforeTheTeardownCompletes pins ADR-0003:S8 for
// EKS DeleteCluster: with the control-plane terminate held, the request still
// returns DELETING, and the record carries the obligation, refs intact, until
// the teardown finishes and removes it.
func TestS8_EKSDeleteClusterReturnsBeforeTheTeardownCompletes(t *testing.T) {
	t.Parallel()
	f := newDeleteClusterFixture(t, "alpha")
	entered, release := blockTerminate(t, f.inst)

	type result struct {
		out *eks.DeleteClusterOutput
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := f.svc.DeleteCluster(context.Background(), deleteInput("alpha"), testAccountID)
		done <- result{out, err}
	}()

	var res result
	select {
	case res = <-done:
	case <-time.After(5 * time.Second):
		release()
		t.Fatal("ADR-0003:S8: DeleteCluster must return without waiting for the control-plane teardown")
	}
	require.NoError(t, res.err)
	assert.Equal(t, string(ClusterStatusDeleting), aws.StringValue(res.out.Cluster.Status))

	waitEntered(t, entered)
	meta, err := GetClusterMeta(t.Context(), f.kv, "alpha")
	require.NoError(t, err, "ADR-0003:S8: the record carries the obligation while the teardown runs")
	assert.Equal(t, ClusterStatusDeleting, meta.Status)
	assert.False(t, meta.DeletingSince.IsZero())
	assert.Equal(t, "i-aaa111", meta.ControlPlaneInstanceID, "the refs the teardown still needs stay recorded")

	release()
	f.svc.WaitTeardowns()
	_, err = GetClusterMeta(t.Context(), f.kv, "alpha")
	assert.ErrorIs(t, err, ErrClusterNotFound, "the teardown removes the record once it completes")
}

// TestS8_EKSSecondDeleteAnswersFromTheRecordWithoutASecondTeardown pins the S8
// rule that a second request for a recorded obligation answers from the record
// and starts no second convergence, even after the first teardown failed.
func TestS8_EKSSecondDeleteAnswersFromTheRecordWithoutASecondTeardown(t *testing.T) {
	t.Parallel()
	f := newDeleteClusterFixture(t, "alpha")
	f.inst.terminateErr = errors.New("hypervisor unreachable")

	_, _ = f.svc.DeleteCluster(context.Background(), deleteInput("alpha"), testAccountID)
	f.svc.WaitTeardowns()
	require.Len(t, f.inst.terminated(), 1)

	out, err := f.svc.DeleteCluster(context.Background(), deleteInput("alpha"), testAccountID)
	require.NoError(t, err, "a repeat delete of a DELETING cluster answers from the record")
	assert.Equal(t, string(ClusterStatusDeleting), aws.StringValue(out.Cluster.Status))
	f.svc.WaitTeardowns()
	assert.Len(t, f.inst.terminated(), 1, "the repeat request must start no teardown of its own")
}

// TestS8_EKSTeardownLeaseOutlivesTheRequest: the teardown lease renews on the
// service's context, so cancelling the request context mid-teardown does not
// let the lease expire and a second node start a duplicate teardown.
func TestS8_EKSTeardownLeaseOutlivesTheRequest(t *testing.T) {
	f := newDeleteClusterFixture(t, "alpha")
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	short, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: "eks-teardown-short-ttl", TTL: time.Second})
	require.NoError(t, err)
	f.svc.leaderKV = short
	origTTL, origRenew := teardownLeaseTTL, teardownLeaseRenew
	teardownLeaseTTL, teardownLeaseRenew = time.Second, 200*time.Millisecond
	t.Cleanup(func() { teardownLeaseTTL, teardownLeaseRenew = origTTL, origRenew })

	entered, release := blockTerminate(t, f.inst)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _, _ = f.svc.DeleteCluster(ctx, deleteInput("alpha"), testAccountID) }()
	waitEntered(t, entered)
	cancel()

	time.Sleep(2500 * time.Millisecond)
	_, err = short.Create(t.Context(), teardownLeaderKey(testAccountID, "alpha"), []byte("node-2"))
	require.ErrorIs(t, err, jetstream.ErrKeyExists,
		"the running teardown must still hold its lease after the request context is gone")

	release()
	f.svc.WaitTeardowns()
}

// TestS8_EKSKickAndSameNodeSweepRunOneTeardown: the teardown lease re-adopts a
// key its own node holds, so only the in-process gate stops a same-node reaper
// sweep from starting a second teardown beside the request's. Each member must
// be terminated exactly once.
func TestS8_EKSKickAndSameNodeSweepRunOneTeardown(t *testing.T) {
	t.Parallel()
	f := newDeleteClusterFixture(t, "alpha")
	seedHAControlPlane(t, f, "alpha")
	entered, release := blockTerminate(t, f.inst)

	go func() { _, _ = f.svc.DeleteCluster(context.Background(), deleteInput("alpha"), testAccountID) }()
	waitEntered(t, entered)

	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		_, _ = (&EKSDeletingReaper{svc: f.svc}).Sweep(context.Background())
	}()
	select {
	case <-sweepDone:
	case <-time.After(3 * time.Second):
	}
	release()
	<-sweepDone
	f.svc.WaitTeardowns()

	got := f.inst.terminated()
	assert.Equal(t, 1, countOf(got, "i-aaa111"), "one teardown per cluster on a node: %v", got)
	assert.Equal(t, 1, countOf(got, "i-bbb222"), "one teardown per cluster on a node: %v", got)
}

// TestS2_EKSTeardownDoesNotSweepARecordChangedDuringTeardown: a member added to
// the record while the teardown runs (a reconciler replacement, a late launch
// write) is torn down by another pass before the record goes. The fixture uses
// the managed CP VPC topology, whose teardown writes the record itself.
func TestS2_EKSTeardownDoesNotSweepARecordChangedDuringTeardown(t *testing.T) {
	t.Parallel()
	f := newDeleteClusterFixture(t, "alpha")
	meta, err := GetClusterMeta(t.Context(), f.kv, "alpha")
	require.NoError(t, err)
	meta.ManagedCPVPC = &ManagedCPVPC{VpcId: "vpc-cp"}
	require.NoError(t, PutClusterMeta(t.Context(), f.kv, meta))
	entered, release := blockTerminate(t, f.inst)

	go func() { _, _ = f.svc.DeleteCluster(context.Background(), deleteInput("alpha"), testAccountID) }()
	waitEntered(t, entered)
	require.NoError(t, casUpdateMeta(t.Context(), f.kv, "alpha", func(m *ClusterMeta) bool {
		m.ControlPlaneNodes = []ControlPlaneNode{
			{InstanceID: "i-aaa111", ENIID: "eni-aaa111"},
			{InstanceID: "i-late", ENIID: "eni-late"},
		}
		return true
	}))
	release()
	f.svc.WaitTeardowns()

	assert.Contains(t, f.inst.terminated(), "i-late", "a member recorded mid-teardown must be torn down, not orphaned")
	_, err = GetClusterMeta(t.Context(), f.kv, "alpha")
	assert.ErrorIs(t, err, ErrClusterNotFound, "the record goes once every recorded member is gone")
}

// TestS2_EKSTeardownRemovesTheMetaLast: the meta owns the cluster's other
// records, so it is deleted only after every sibling key is gone.
func TestS2_EKSTeardownRemovesTheMetaLast(t *testing.T) {
	t.Parallel()
	_, nc, _ := testutil.StartTestJetStream(t)
	kv, err := GetOrCreateAccountBucket(t.Context(), testutil.NewJetStream(t, nc), testAccountID)
	require.NoError(t, err)
	require.NoError(t, PutClusterMeta(t.Context(), kv, sampleClusterMeta("alpha")))
	_, err = kv.Put(t.Context(), "clusters/alpha/addons/vpc-cni", []byte(`{}`))
	require.NoError(t, err)

	faulty := &failDeleteKV{KeyValue: kv, fail: "clusters/alpha/addons/vpc-cni"}
	require.Error(t, DeleteClusterPrefix(t.Context(), faulty, "alpha"))

	_, err = GetClusterMeta(t.Context(), kv, "alpha")
	require.NoError(t, err, "ADR-0003:S2: the meta must survive a sweep that could not remove its children")
}

type failDeleteKV struct {
	jetstream.KeyValue

	fail string
}

func (f *failDeleteKV) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	if key == f.fail {
		return errors.New("injected delete failure")
	}
	return f.KeyValue.Delete(ctx, key, opts...)
}

// TestS2_EKSTeardownResumesAfterAnInterruptedConverge pins crash recovery: a
// teardown stopped between members (its lease lost) keeps the record, and the
// reaper's next pass finishes it with no step failing on what is already gone.
func TestS2_EKSTeardownResumesAfterAnInterruptedConverge(t *testing.T) {
	t.Parallel()
	f := newDeleteClusterFixture(t, "alpha")
	seedHAControlPlane(t, f, "alpha")
	_, _, err := MarkClusterDeleting(t.Context(), f.kv, "alpha")
	require.NoError(t, err)

	lost := make(chan struct{})
	var once sync.Once
	f.inst.terminateHook = func(string) { once.Do(func() { close(lost) }) }
	err = f.svc.convergeClusterDeletion(t.Context(), testAccountID, "alpha", lost)
	require.ErrorIs(t, err, errTeardownLeaseLost)
	assert.Equal(t, []string{"i-aaa111"}, f.inst.terminated(), "the teardown stops at the next step boundary")
	meta, err := GetClusterMeta(t.Context(), f.kv, "alpha")
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusDeleting, meta.Status)

	f.inst.terminateHook = nil
	redrive(t, f.svc)
	_, err = GetClusterMeta(t.Context(), f.kv, "alpha")
	require.ErrorIs(t, err, ErrClusterNotFound)
	assert.Contains(t, f.inst.terminated(), "i-bbb222")
}

// TestS7_EKSTeardownActsOnlyOnARecordedDelete: the teardown terminates
// control-plane instances and their volumes, so it may act only on the
// tenant's recorded decision. Driven directly and through the reaper, it
// touches nothing on an ACTIVE, CREATING or FAILED record.
func TestS7_EKSTeardownActsOnlyOnARecordedDelete(t *testing.T) {
	t.Parallel()
	for _, status := range []ClusterStatus{ClusterStatusActive, ClusterStatusCreating, ClusterStatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			f := newDeleteClusterFixture(t, "alpha")
			meta, err := GetClusterMeta(t.Context(), f.kv, "alpha")
			require.NoError(t, err)
			meta.Status = status
			require.NoError(t, PutClusterMeta(t.Context(), f.kv, meta))

			err = f.svc.convergeClusterDeletion(t.Context(), testAccountID, "alpha", nil)
			require.ErrorIs(t, err, errTeardownNotRecorded)
			redrive(t, f.svc)

			assert.Empty(t, f.inst.terminated(), "ADR-0003:S7: no instance is terminated without a recorded delete")
			assert.Empty(t, f.eip.releaseCalls)
			assert.Empty(t, f.nlb.deleteLBCalls)
			after, err := GetClusterMeta(t.Context(), f.kv, "alpha")
			require.NoError(t, err)
			assert.Equal(t, status, after.Status)
		})
	}
}

// TestS7_EKSTeardownNeverRemovesAClusterWithNodegroupWorkers: the cluster
// teardown never terminates node group workers and never erases their record,
// which would orphan running customer instances. A DELETING cluster that still
// has a node group is left whole, with the wait surfaced to the tenant.
func TestS7_EKSTeardownNeverRemovesAClusterWithNodegroupWorkers(t *testing.T) {
	t.Parallel()
	f := newDeleteClusterFixture(t, "alpha")
	markDeleting(t, f, "alpha", 10*time.Minute)
	require.NoError(t, PutNodegroupRecord(t.Context(), f.kv, &NodegroupRecord{
		ClusterName: "alpha", Name: "ng1", Status: eks.NodegroupStatusActive,
		InstanceIDs: []string{"i-worker1"},
	}))

	redrive(t, f.svc)

	assert.Empty(t, f.inst.terminated(), "the control plane stays while workers depend on it")
	_, err := GetNodegroupRecord(t.Context(), f.kv, "alpha", "ng1")
	require.NoError(t, err, "ADR-0003:S7: the node group record that owns running workers must survive")
	meta, err := GetClusterMeta(t.Context(), f.kv, "alpha")
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusDeleting, meta.Status)
	assert.Equal(t, stepNodegroups, meta.DeleteFailedStep)
}

// TestS2_EKSLaunchWriteAfterDeleteNeitherRevertsNorRecreates: a create launch
// still running when the delete is recorded must not move the record off
// DELETING; its refs join the record so the teardown reaches them. Once the
// record is gone, the launch's write must not bring it back.
func TestS2_EKSLaunchWriteAfterDeleteNeitherRevertsNorRecreates(t *testing.T) {
	t.Parallel()

	t.Run("record still DELETING", func(t *testing.T) {
		t.Parallel()
		f := newEKSServiceFixture(t)
		held := make(chan struct{})
		gate := make(chan struct{})
		var once sync.Once
		f.inst.launchHook = func() { once.Do(func() { close(held); <-gate }) }
		f.nlb.deleteLBErr = errors.New("elbv2 unavailable") // keep the record DELETING

		_, err := f.svc.CreateCluster(context.Background(), createInput("alpha"), testAccountID, "")
		require.NoError(t, err)
		<-held
		_, _ = f.svc.DeleteCluster(context.Background(), deleteInput("alpha"), testAccountID)
		f.svc.WaitTeardowns()
		close(gate)
		f.svc.WaitLaunches()

		meta, err := GetClusterMeta(t.Context(), f.kv, "alpha")
		require.NoError(t, err)
		assert.Equal(t, ClusterStatusDeleting, meta.Status, "a launch write must never move a cluster off DELETING")
		assert.NotEmpty(t, meta.ControlPlaneInstanceID, "the launch's control-plane VM joins the record so the teardown reaches it")
	})

	t.Run("record already removed", func(t *testing.T) {
		t.Parallel()
		f := newEKSServiceFixture(t)
		held := make(chan struct{})
		gate := make(chan struct{})
		var once sync.Once
		f.inst.launchHook = func() { once.Do(func() { close(held); <-gate }) }

		_, err := f.svc.CreateCluster(context.Background(), createInput("alpha"), testAccountID, "")
		require.NoError(t, err)
		<-held
		_, _ = f.svc.DeleteCluster(context.Background(), deleteInput("alpha"), testAccountID)
		f.svc.WaitTeardowns()
		_, err = GetClusterMeta(t.Context(), f.kv, "alpha")
		require.ErrorIs(t, err, ErrClusterNotFound)
		close(gate)
		f.svc.WaitLaunches()

		_, err = GetClusterMeta(t.Context(), f.kv, "alpha")
		assert.ErrorIs(t, err, ErrClusterNotFound, "a launch must never recreate a swept record")
		assert.Contains(t, f.inst.terminated(), "i-aaa111", "the launch releases the VM it started after the record went")
	})
}

// TestS2_EKSDeletingIsStickyForStatusWriters: no status writer moves a record
// off DELETING.
func TestS2_EKSDeletingIsStickyForStatusWriters(t *testing.T) {
	t.Parallel()
	f := newDeleteClusterFixture(t, "alpha")
	_, _, err := MarkClusterDeleting(t.Context(), f.kv, "alpha")
	require.NoError(t, err)

	for _, status := range []ClusterStatus{ClusterStatusActive, ClusterStatusFailed, ClusterStatusCreating} {
		err := SetClusterStatus(t.Context(), f.kv, "alpha", status)
		require.ErrorIs(t, err, ErrClusterDeleting, "moving DELETING to %s must be refused", status)
	}
	meta, err := GetClusterMeta(t.Context(), f.kv, "alpha")
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusDeleting, meta.Status)
}

// TestDescribeCluster_StuckDeleteShowsTheStepNotTheError: a failed teardown is
// visible to the tenant as one INTERNAL_FAILURE issue naming the step, never
// the internal error with its system-account IDs.
func TestDescribeCluster_StuckDeleteShowsTheStepNotTheError(t *testing.T) {
	t.Parallel()
	meta := sampleClusterMeta("alpha")
	meta.Status = ClusterStatusDeleting
	meta.DeleteFailedStep = stepControlPlane
	meta.LastDeleteReapError = "terminate K3s VM i-0123: route terminate: nats: timeout"

	out := clusterMetaToAWS(meta)
	require.NotNil(t, out.Health)
	require.Len(t, out.Health.Issues, 1)
	issue := out.Health.Issues[0]
	assert.Equal(t, eks.ClusterIssueCodeInternalFailure, aws.StringValue(issue.Code))
	assert.Contains(t, aws.StringValue(issue.Message), stepControlPlane)
	assert.NotContains(t, aws.StringValue(issue.Message), "i-0123")
	assert.NotContains(t, aws.StringValue(issue.Message), "nats")

	meta.Status = ClusterStatusActive
	assert.Nil(t, clusterMetaToAWS(meta).Health, "only a DELETING cluster reports a stuck delete")
}
