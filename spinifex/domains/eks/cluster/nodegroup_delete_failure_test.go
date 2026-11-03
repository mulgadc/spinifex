package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (f *fakeWorkerLauncher) terminateCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.terminateCalls)
}

func requireNodegroupStatus(t *testing.T, f *eksServiceFixture, cluster, ng, want string) *NodegroupRecord {
	t.Helper()
	rec, err := GetNodegroupRecord(t.Context(), f.kv, cluster, ng)
	require.NoError(t, err)
	require.Equal(t, want, rec.Status)
	return rec
}

func TestNodegroupDeletingReaper_SweepWithoutOrchestrationDepsStartsNothing(t *testing.T) {
	t.Parallel()
	n, err := (&EKSServiceImpl{}).NewNodegroupDeletingReaper().Sweep(t.Context())
	require.NoError(t, err)
	assert.Zero(t, n)
}

func TestNodegroupDeletingReaper_CancelledSweepLeavesTheDeleteRecorded(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	createReadyNodegroup(t, f, "c1", "ng1", 1)
	_, err := deleteNodegroupCall(f, "c1", "ng1")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	n, err := f.svc.NewNodegroupDeletingReaper().Sweep(ctx)
	f.svc.WaitNodegroupDeletes()

	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, n)
	requireNodegroupStatus(t, f, "c1", "ng1", eks.NodegroupStatusDeleting)
}

func TestNodegroupDeletingReaper_OnlyDeletingGroupsPastBackoffAreStarted(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	createReadyNodegroup(t, f, "c1", "active", 1)
	createReadyNodegroup(t, f, "c1", "backoff", 1)
	_, err := deleteNodegroupCall(f, "c1", "backoff")
	require.NoError(t, err)
	rec := requireNodegroupStatus(t, f, "c1", "backoff", eks.NodegroupStatusDeleting)
	rec.DeleteReapAttempts = 3
	rec.LastDeleteReapAttempt = time.Now().UTC()
	require.NoError(t, PutNodegroupRecord(t.Context(), f.kv, rec))

	assert.Zero(t, sweepNodegroups(t, f), "an ACTIVE group and a DELETING one inside its backoff are not due")
	requireNodegroupStatus(t, f, "c1", "active", eks.NodegroupStatusActive)
	assert.Zero(t, f.worker.terminateCallCount())
}

func TestConvergeNodegroupDeletion_MissingOrNotDeletingRecordIsLeftAlone(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	createReadyNodegroup(t, f, "c1", "ng1", 1)

	require.NoError(t, f.svc.convergeNodegroupDeletion(t.Context(), testAccountID, f.kv, "c1", "never-existed"))
	require.NoError(t, f.svc.convergeNodegroupDeletion(t.Context(), testAccountID, f.kv, "c1", "ng1"))

	requireNodegroupStatus(t, f, "c1", "ng1", eks.NodegroupStatusActive)
	assert.Zero(t, f.worker.terminateCallCount(), "a group nobody asked to delete keeps its workers")
	assert.Contains(t, fixtureIAM(t, f).Profiles, mustProfileName(t, f, "c1", "ng1"))
}

func mustProfileName(t *testing.T, f *eksServiceFixture, cluster, ng string) string {
	t.Helper()
	rec, _ := ownedProfile(t, f, cluster, ng)
	return rec.InstanceProfileName
}

func TestAwaitWorkersGone_CancelledWhileWorkersEndingReturnsTheContextError(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	f.worker.stillLive = []string{"i-slow"}
	f.svc.workersEndWait = time.Hour
	f.svc.workersEndPoll = time.Hour
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(50*time.Millisecond, cancel)

	err := f.svc.awaitWorkersGone(ctx, []string{"i-slow"}, testAccountID)

	require.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, errWorkersEnding, "a cancelled wait is not a verdict on the workers")
}

func TestDeleteNodegroup_IAMReadFailureKeepsGroupDeletingAndProfileIntact(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	createReadyNodegroup(t, f, "c1", "ng1", 1)
	rec, _ := ownedProfile(t, f, "c1", "ng1")
	m := fixtureIAM(t, f)
	m.GetInstanceProfileErr = errors.New("iam read timed out")
	_, err := deleteNodegroupCall(f, "c1", "ng1")
	require.NoError(t, err)

	sweepNodegroups(t, f)

	cur := requireNodegroupStatus(t, f, "c1", "ng1", eks.NodegroupStatusDeleting)
	assert.Contains(t, cur.StatusReason, "get instance profile", "the failed read is recorded for the customer")
	assert.Contains(t, m.Profiles, rec.InstanceProfileName, "an unreadable profile is not a missing one")
	assert.Zero(t, m.DeleteInstanceProfileCalls)

	m.GetInstanceProfileErr = nil
	backdateNodegroupReap(t, f, "c1", "ng1")
	sweepNodegroups(t, f)
	_, err = describeNodegroupCall(f, "c1", "ng1")
	requireErrorCode(t, err, awserrors.ErrorEKSResourceNotFound)
	assert.NotContains(t, m.Profiles, rec.InstanceProfileName)
}

func TestDeleteNodegroup_ProfileAlreadyGoneStillFinishesTheGroup(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	createReadyNodegroup(t, f, "c1", "ng1", 1)
	rec, _ := ownedProfile(t, f, "c1", "ng1")
	m := fixtureIAM(t, f)
	delete(m.Profiles, rec.InstanceProfileName)
	_, err := deleteNodegroupCall(f, "c1", "ng1")
	require.NoError(t, err)

	sweepNodegroups(t, f)

	_, err = describeNodegroupCall(f, "c1", "ng1")
	requireErrorCode(t, err, awserrors.ErrorEKSResourceNotFound)
	assert.Zero(t, m.RemoveRoleCalls)
	assert.Zero(t, m.DeleteInstanceProfileCalls)
}

func TestDeleteNodegroup_ProfileVanishingMidDeleteStillFinishesTheGroup(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	createReadyNodegroup(t, f, "c1", "ng1", 1)
	m := fixtureIAM(t, f)
	m.DeleteInstanceProfileErr = awserrors.Errorf(awserrors.ErrorIAMNoSuchEntity, "Instance Profile gone.")
	_, err := deleteNodegroupCall(f, "c1", "ng1")
	require.NoError(t, err)

	sweepNodegroups(t, f)

	_, err = describeNodegroupCall(f, "c1", "ng1")
	requireErrorCode(t, err, awserrors.ErrorEKSResourceNotFound)
	assert.Equal(t, 1, m.RemoveRoleCalls)
}

func TestDeleteNodegroup_DeleteProfileFailureKeepsGroupDeletingUntilItClears(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	createReadyNodegroup(t, f, "c1", "ng1", 1)
	rec, _ := ownedProfile(t, f, "c1", "ng1")
	m := fixtureIAM(t, f)
	m.DeleteInstanceProfileErr = errors.New("iam unavailable")
	_, err := deleteNodegroupCall(f, "c1", "ng1")
	require.NoError(t, err)

	sweepNodegroups(t, f)
	cur := requireNodegroupStatus(t, f, "c1", "ng1", eks.NodegroupStatusDeleting)
	assert.Contains(t, cur.StatusReason, "delete instance profile")
	assert.Empty(t, cur.InstanceIDs, "workers proven gone are not terminated again")

	m.DeleteInstanceProfileErr = nil
	backdateNodegroupReap(t, f, "c1", "ng1")
	sweepNodegroups(t, f)
	_, err = describeNodegroupCall(f, "c1", "ng1")
	requireErrorCode(t, err, awserrors.ErrorEKSResourceNotFound)
	assert.NotContains(t, m.Profiles, rec.InstanceProfileName)
}

func TestEnsureNodegroupProfile_RecreatesAMissingRecordedProfileAsItsOwn(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	createReadyNodegroup(t, f, "c1", "ng1", 1)
	rec, _ := ownedProfile(t, f, "c1", "ng1")
	m := fixtureIAM(t, f)
	delete(m.Profiles, rec.InstanceProfileName)

	arn, err := f.svc.ensureNodegroupProfile(testAccountID, rec)

	require.NoError(t, err)
	assert.Contains(t, arn, rec.InstanceProfileName)
	got := m.Profiles[rec.InstanceProfileName]
	require.NotNil(t, got)
	assert.True(t, profileOwnedBy(got, rec), "a re-created profile carries this incarnation's tags")
	assert.Equal(t, []string{fixtureNodeRoleName}, profileRoleNames(got))
}

func TestEnsureNodegroupProfile_RefusesAProfileThatIsNotThisGroups(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	createReadyNodegroup(t, f, "c1", "ng1", 1)
	rec, p := ownedProfile(t, f, "c1", "ng1")
	p.Tags = nil
	callsBefore := fixtureIAM(t, f).AddRoleToInstanceProfileCalls

	_, err := f.svc.ensureNodegroupProfile(testAccountID, rec)

	require.ErrorContains(t, err, "is not this nodegroup's")
	assert.Equal(t, callsBefore, fixtureIAM(t, f).AddRoleToInstanceProfileCalls, "no role is attached to a profile EKS does not own")
}

func TestEnsureNodegroupProfile_IAMFailuresSurfaceWithoutHalfWork(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	createReadyNodegroup(t, f, "c1", "ng1", 1)
	rec, _ := ownedProfile(t, f, "c1", "ng1")
	m := fixtureIAM(t, f)

	m.GetInstanceProfileErr = errors.New("iam read failed")
	_, err := f.svc.ensureNodegroupProfile(testAccountID, rec)
	require.ErrorContains(t, err, "get instance profile")
	m.GetInstanceProfileErr = nil

	delete(m.Profiles, rec.InstanceProfileName)
	m.CreateInstanceProfileErr = errors.New("iam write failed")
	_, err = f.svc.ensureNodegroupProfile(testAccountID, rec)
	require.ErrorContains(t, err, "create instance profile")
	assert.NotContains(t, m.Profiles, rec.InstanceProfileName)
}

func TestMarkNodegroupDeleting_UnknownGroupIsResourceNotFound(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")

	_, err := f.svc.markNodegroupDeleting(t.Context(), f.kv, "c1", "ghost")

	requireErrorCode(t, err, awserrors.ErrorEKSResourceNotFound)
}
