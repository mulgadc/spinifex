package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/aws/aws-sdk-go/service/iam"
	iammock "github.com/mulgadc/spinifex/spinifex/domains/iam/mock"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	profileClusterTag   = "spinifex:eks-cluster"
	profileNodegroupTag = "spinifex:eks-nodegroup"
	fixtureNodeRoleName = "eks-node"
)

func fixtureIAM(t *testing.T, f *eksServiceFixture) *iammock.SystemInstanceRoleEnsurer {
	t.Helper()
	m, ok := f.svc.deps.IAM.(*iammock.SystemInstanceRoleEnsurer)
	require.True(t, ok, "the fixture IAM must be the in-memory mock")
	return m
}

// createReadyNodegroup creates a nodegroup whose n workers register Ready.
func createReadyNodegroup(t *testing.T, f *eksServiceFixture, cluster, ng string, n int64) {
	t.Helper()
	markWorkersReady(t, f, cluster, ng, int(n))
	_, err := f.svc.CreateNodegroup(context.Background(), createNGInput(cluster, ng, n), testAccountID)
	require.NoError(t, err)
	f.svc.WaitLaunches()
}

// createUnreadyNodegroup creates a nodegroup that never registers, so it ends
// CREATE_FAILED holding the workers it launched.
func createUnreadyNodegroup(t *testing.T, f *eksServiceFixture, cluster, ng string, n int64) {
	t.Helper()
	_, err := f.svc.CreateNodegroup(context.Background(), createNGInput(cluster, ng, n), testAccountID)
	require.NoError(t, err)
	f.svc.WaitLaunches()
}

func deleteNodegroupCall(f *eksServiceFixture, cluster, ng string) (*eks.DeleteNodegroupOutput, error) {
	return f.svc.DeleteNodegroup(context.Background(), &eks.DeleteNodegroupInput{
		ClusterName: aws.String(cluster), NodegroupName: aws.String(ng),
	}, testAccountID)
}

func describeNodegroupCall(f *eksServiceFixture, cluster, ng string) (*eks.DescribeNodegroupOutput, error) {
	return f.svc.DescribeNodegroup(context.Background(), &eks.DescribeNodegroupInput{
		ClusterName: aws.String(cluster), NodegroupName: aws.String(ng),
	}, testAccountID)
}

func sweepNodegroups(t *testing.T, f *eksServiceFixture) int {
	t.Helper()
	n, err := f.svc.NewNodegroupDeletingReaper().Sweep(context.Background())
	require.NoError(t, err)
	f.svc.WaitNodegroupDeletes()
	return n
}

// backdateNodegroupReap moves the last reap pass far enough back that any
// retry backoff has elapsed.
func backdateNodegroupReap(t *testing.T, f *eksServiceFixture, cluster, ng string) {
	t.Helper()
	rec, err := GetNodegroupRecord(t.Context(), f.kv, cluster, ng)
	require.NoError(t, err)
	rec.LastDeleteReapAttempt = time.Now().UTC().Add(-24 * time.Hour)
	rec.DeletingSince = time.Now().UTC().Add(-24 * time.Hour)
	require.NoError(t, PutNodegroupRecord(t.Context(), f.kv, rec))
}

func requireErrorCode(t *testing.T, err error, want string) {
	t.Helper()
	require.Error(t, err)
	code, ok := awserrors.ResolveErrorCode(err)
	require.True(t, ok, "error %v carries no AWS code", err)
	assert.Equal(t, want, code)
}

func profileTag(p *iam.InstanceProfile, key string) string {
	for _, tg := range p.Tags {
		if aws.StringValue(tg.Key) == key {
			return aws.StringValue(tg.Value)
		}
	}
	return ""
}

func profileHasTagValue(p *iam.InstanceProfile, value string) bool {
	for _, tg := range p.Tags {
		if aws.StringValue(tg.Value) == value {
			return true
		}
	}
	return false
}

func profileRoleNames(p *iam.InstanceProfile) []string {
	var names []string
	for _, r := range p.Roles {
		names = append(names, aws.StringValue(r.RoleName))
	}
	return names
}

// ownedProfile returns the nodegroup's record and the instance profile it
// names, failing the test if either is missing.
func ownedProfile(t *testing.T, f *eksServiceFixture, cluster, ng string) (*NodegroupRecord, *iam.InstanceProfile) {
	t.Helper()
	rec, err := GetNodegroupRecord(t.Context(), f.kv, cluster, ng)
	require.NoError(t, err)
	require.NotEmpty(t, rec.InstanceProfileName, "the record must name the profile it owns")
	require.NotEmpty(t, rec.InstanceProfileID, "the record must carry the per-incarnation id")
	p, ok := fixtureIAM(t, f).Profiles[rec.InstanceProfileName]
	require.True(t, ok, "profile %q must exist", rec.InstanceProfileName)
	return rec, p
}

func seedCustomerProfile(f *eksServiceFixture, t *testing.T, name, role string, tags map[string]string) {
	t.Helper()
	p := &iam.InstanceProfile{
		InstanceProfileName: aws.String(name),
		Arn:                 aws.String("arn:aws:iam::" + testAccountID + ":instance-profile/" + name),
	}
	if role != "" {
		p.Roles = []*iam.Role{{RoleName: aws.String(role)}}
	}
	for k, v := range tags {
		p.Tags = append(p.Tags, &iam.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	fixtureIAM(t, f).Profiles[name] = p
}

func TestDeleteNodegroup_ReturnsDeletingWithoutTerminatingInRequest(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	createReadyNodegroup(t, f, "c1", "ng1", 2)

	out, err := deleteNodegroupCall(f, "c1", "ng1")
	require.NoError(t, err)
	assert.Equal(t, eks.NodegroupStatusDeleting, aws.StringValue(out.Nodegroup.Status))

	assert.Zero(t, f.worker.terminatedCount(), "the request must not wait for workers to end")
	rec, err := GetNodegroupRecord(t.Context(), f.kv, "c1", "ng1")
	require.NoError(t, err, "the record stays until the reaper has finished")
	assert.Equal(t, eks.NodegroupStatusDeleting, rec.Status)

	desc, err := describeNodegroupCall(f, "c1", "ng1")
	require.NoError(t, err)
	assert.Equal(t, eks.NodegroupStatusDeleting, aws.StringValue(desc.Nodegroup.Status))
}

func TestDeleteNodegroup_ReaperEndsWorkersThenRemovesGroup(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	createReadyNodegroup(t, f, "c1", "ng1", 2)
	rec, _ := ownedProfile(t, f, "c1", "ng1")

	_, err := deleteNodegroupCall(f, "c1", "ng1")
	require.NoError(t, err)
	assert.Equal(t, 1, sweepNodegroups(t, f))

	require.Len(t, f.worker.terminateCalls, 1)
	assert.ElementsMatch(t, rec.InstanceIDs, f.worker.terminateCalls[0])
	_, err = describeNodegroupCall(f, "c1", "ng1")
	requireErrorCode(t, err, awserrors.ErrorEKSResourceNotFound)
	_, ok := fixtureIAM(t, f).Profiles[rec.InstanceProfileName]
	assert.False(t, ok, "the group is not gone until its profile is")
}

func TestDeleteNodegroup_RepeatedDeleteOnDeletingGroupIsResourceInUse(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	createReadyNodegroup(t, f, "c1", "ng1", 1)
	_, err := deleteNodegroupCall(f, "c1", "ng1")
	require.NoError(t, err)

	_, err = deleteNodegroupCall(f, "c1", "ng1")
	requireErrorCode(t, err, awserrors.ErrorEKSResourceInUse)
}

func TestDeleteNodegroup_DeletingGroupRefusesUpdates(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	createReadyNodegroup(t, f, "c1", "ng1", 1)
	_, err := deleteNodegroupCall(f, "c1", "ng1")
	require.NoError(t, err)

	_, err = f.svc.UpdateNodegroupConfig(context.Background(), &eks.UpdateNodegroupConfigInput{
		ClusterName:   aws.String("c1"),
		NodegroupName: aws.String("ng1"),
		ScalingConfig: &eks.NodegroupScalingConfig{MinSize: aws.Int64(1), MaxSize: aws.Int64(3), DesiredSize: aws.Int64(3)},
	}, testAccountID)
	requireErrorCode(t, err, awserrors.ErrorEKSResourceInUse)
	assert.Equal(t, 1, f.worker.runCount(), "a DELETING group must not launch workers")
}

func TestDeleteNodegroup_SameNameCreateWhileDeletingIsResourceInUse(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	createReadyNodegroup(t, f, "c1", "ng1", 1)
	_, err := deleteNodegroupCall(f, "c1", "ng1")
	require.NoError(t, err)

	_, err = f.svc.CreateNodegroup(context.Background(), createNGInput("c1", "ng1", 1), testAccountID)
	requireErrorCode(t, err, awserrors.ErrorEKSResourceInUse)
}

func TestDeleteNodegroup_DeleteClusterRefusedWhileGroupDeleting(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	createReadyNodegroup(t, f, "c1", "ng1", 1)
	_, err := deleteNodegroupCall(f, "c1", "ng1")
	require.NoError(t, err)

	_, err = f.svc.DeleteCluster(context.Background(), &eks.DeleteClusterInput{Name: aws.String("c1")}, testAccountID)
	requireErrorCode(t, err, awserrors.ErrorEKSResourceInUse)
}

func TestDeleteNodegroup_BootReclaimLeavesDeletingGroupToReaper(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	require.NoError(t, PutNodegroupRecord(t.Context(), f.kv, &NodegroupRecord{
		ClusterName: "c1", Name: "ng1", Status: eks.NodegroupStatusDeleting,
		InstanceIDs: []string{"i-w1", "i-w2"}, NodeRole: fixtureNodeRoleARN,
	}))

	f.svc.reclaimOrphanedNodegroups(t.Context(), testAccountID, f.kv, "c1")

	assert.Zero(t, f.worker.terminatedCount())
	rec, err := GetNodegroupRecord(t.Context(), f.kv, "c1", "ng1")
	require.NoError(t, err)
	assert.Equal(t, eks.NodegroupStatusDeleting, rec.Status)
	assert.Equal(t, []string{"i-w1", "i-w2"}, rec.InstanceIDs)
}

func TestDeleteNodegroup_RepeatedReaperFailureEndsDeleteFailedWithHealthIssue(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	createReadyNodegroup(t, f, "c1", "ng1", 1)
	f.worker.termErr = errors.New("terminate refused")
	_, err := deleteNodegroupCall(f, "c1", "ng1")
	require.NoError(t, err)

	var rec *NodegroupRecord
	for range 30 {
		sweepNodegroups(t, f)
		rec, err = GetNodegroupRecord(t.Context(), f.kv, "c1", "ng1")
		require.NoError(t, err, "a failing delete must leave the record, never drop it")
		if rec.Status == eks.NodegroupStatusDeleteFailed {
			break
		}
		backdateNodegroupReap(t, f, "c1", "ng1")
	}
	require.Equal(t, eks.NodegroupStatusDeleteFailed, rec.Status)

	desc, err := describeNodegroupCall(f, "c1", "ng1")
	require.NoError(t, err)
	require.NotEmpty(t, desc.Nodegroup.Health.Issues, "DELETE_FAILED must say why")
	assert.Equal(t, eks.NodegroupIssueCodeNodeTerminationFailure, aws.StringValue(desc.Nodegroup.Health.Issues[0].Code))

	// The customer retries the delete once the cause is gone.
	f.worker.termErr = nil
	_, err = deleteNodegroupCall(f, "c1", "ng1")
	require.NoError(t, err)
	sweepNodegroups(t, f)
	_, err = describeNodegroupCall(f, "c1", "ng1")
	requireErrorCode(t, err, awserrors.ErrorEKSResourceNotFound)
}

func TestCreateNodegroup_OwnsATaggedProfileRecordedBeforeFirstLaunch(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	var seenAtLaunch string
	f.worker.beforeRun = func(in *ec2.RunInstancesInput) {
		if rec, err := GetNodegroupRecord(t.Context(), f.kv, "c1", "ng1"); err == nil && seenAtLaunch == "" {
			seenAtLaunch = rec.InstanceProfileName
		}
	}
	createReadyNodegroup(t, f, "c1", "ng1", 2)

	rec, p := ownedProfile(t, f, "c1", "ng1")
	assert.NotEqual(t, fixtureNodeRoleName, rec.InstanceProfileName, "the profile is the group's own, not the role-named one")
	assert.Equal(t, rec.InstanceProfileName, seenAtLaunch, "the name must be durable before any worker launches")
	assert.Equal(t, "c1", profileTag(p, profileClusterTag))
	assert.Equal(t, "ng1", profileTag(p, profileNodegroupTag))
	assert.True(t, profileHasTagValue(p, rec.InstanceProfileID), "a tag must carry the incarnation id")
	for _, tg := range p.Tags {
		assert.True(t, strings.HasPrefix(aws.StringValue(tg.Key), "spinifex:"), "tag %q: aws: keys are reserved, ownership tags must be spinifex:", aws.StringValue(tg.Key))
	}
	assert.Equal(t, []string{fixtureNodeRoleName}, profileRoleNames(p))

	require.Equal(t, 2, f.worker.runCount())
	for _, in := range f.worker.runCalls {
		require.NotNil(t, in.IamInstanceProfile)
		assert.True(t, strings.HasSuffix(aws.StringValue(in.IamInstanceProfile.Arn), "/"+rec.InstanceProfileName),
			"every worker launches with the group's own profile, got %s", aws.StringValue(in.IamInstanceProfile.Arn))
	}
}

func TestCreateNodegroup_TwoGroupsOnOneRoleGetTwoProfiles(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	createUnreadyNodegroup(t, f, "c1", "ng-a", 1)
	createUnreadyNodegroup(t, f, "c1", "ng-b", 1)

	recA, pA := ownedProfile(t, f, "c1", "ng-a")
	recB, pB := ownedProfile(t, f, "c1", "ng-b")
	assert.NotEqual(t, recA.InstanceProfileName, recB.InstanceProfileName)
	assert.NotEqual(t, recA.InstanceProfileID, recB.InstanceProfileID)
	assert.Equal(t, []string{fixtureNodeRoleName}, profileRoleNames(pA))
	assert.Equal(t, []string{fixtureNodeRoleName}, profileRoleNames(pB))
}

func TestCreateNodegroup_RecreatedGroupOfSameNameGetsANewProfile(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	createUnreadyNodegroup(t, f, "c1", "ng1", 1)
	first, _ := ownedProfile(t, f, "c1", "ng1")
	_, err := deleteNodegroupCall(f, "c1", "ng1")
	require.NoError(t, err)
	sweepNodegroups(t, f)

	createUnreadyNodegroup(t, f, "c1", "ng1", 1)
	second, _ := ownedProfile(t, f, "c1", "ng1")
	assert.NotEqual(t, first.InstanceProfileName, second.InstanceProfileName)
	assert.NotEqual(t, first.InstanceProfileID, second.InstanceProfileID)
	_, stillThere := fixtureIAM(t, f).Profiles[first.InstanceProfileName]
	assert.False(t, stillThere, "the first incarnation's profile must have gone with it")
}

func TestDeleteNodegroup_AfterCreateFailedRemovesProfileAndGroup(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	createUnreadyNodegroup(t, f, "c1", "ng1", 2)
	rec, _ := ownedProfile(t, f, "c1", "ng1")
	require.Equal(t, eks.NodegroupStatusCreateFailed, rec.Status)

	_, err := deleteNodegroupCall(f, "c1", "ng1")
	require.NoError(t, err)
	sweepNodegroups(t, f)

	_, ok := fixtureIAM(t, f).Profiles[rec.InstanceProfileName]
	assert.False(t, ok)
	assert.Equal(t, 2, f.worker.terminatedCount())
	_, err = describeNodegroupCall(f, "c1", "ng1")
	requireErrorCode(t, err, awserrors.ErrorEKSResourceNotFound)
}

func TestDeleteNodegroup_ProfileSurvivesWhileWorkersAreLive(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	createReadyNodegroup(t, f, "c1", "ng1", 1)
	rec, _ := ownedProfile(t, f, "c1", "ng1")
	f.worker.termErr = errors.New("terminate refused")
	_, err := deleteNodegroupCall(f, "c1", "ng1")
	require.NoError(t, err)

	sweepNodegroups(t, f)

	m := fixtureIAM(t, f)
	assert.Contains(t, m.Profiles, rec.InstanceProfileName, "a profile in use by live workers must not be touched")
	assert.Zero(t, m.RemoveRoleCalls)
	assert.Zero(t, m.DeleteInstanceProfileCalls)
}

func TestDeleteNodegroup_PartialProfileDeleteConvergesOnRetry(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	createReadyNodegroup(t, f, "c1", "ng1", 1)
	rec, _ := ownedProfile(t, f, "c1", "ng1")
	m := fixtureIAM(t, f)
	m.RemoveRoleFromInstanceProfileErr = errors.New("iam unavailable")
	_, err := deleteNodegroupCall(f, "c1", "ng1")
	require.NoError(t, err)

	sweepNodegroups(t, f)
	still, err := GetNodegroupRecord(t.Context(), f.kv, "c1", "ng1")
	require.NoError(t, err, "the group is not gone while its profile is")
	assert.Equal(t, eks.NodegroupStatusDeleting, still.Status)

	m.RemoveRoleFromInstanceProfileErr = nil
	backdateNodegroupReap(t, f, "c1", "ng1")
	sweepNodegroups(t, f)
	assert.NotContains(t, m.Profiles, rec.InstanceProfileName)
	_, err = describeNodegroupCall(f, "c1", "ng1")
	requireErrorCode(t, err, awserrors.ErrorEKSResourceNotFound)
}

func TestDeleteNodegroup_CustomerRoleNamedProfileSurvives(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	seedCustomerProfile(f, t, fixtureNodeRoleName, fixtureNodeRoleName, nil)
	createReadyNodegroup(t, f, "c1", "ng1", 1)
	rec, _ := ownedProfile(t, f, "c1", "ng1")

	_, err := deleteNodegroupCall(f, "c1", "ng1")
	require.NoError(t, err)
	sweepNodegroups(t, f)

	m := fixtureIAM(t, f)
	assert.NotContains(t, m.Profiles, rec.InstanceProfileName, "the group's own profile goes")
	require.Contains(t, m.Profiles, fixtureNodeRoleName, "a customer's profile is never EKS's to delete")
	assert.Equal(t, []string{fixtureNodeRoleName}, profileRoleNames(m.Profiles[fixtureNodeRoleName]))
	assert.Equal(t, 1, m.DeleteInstanceProfileCalls)
}

func TestDeleteNodegroup_RecordedProfileMissingOwnershipTagSurvives(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	createReadyNodegroup(t, f, "c1", "ng1", 1)
	rec, _ := ownedProfile(t, f, "c1", "ng1")
	m := fixtureIAM(t, f)
	_, err := m.UntagInstanceProfile(testAccountID, &iam.UntagInstanceProfileInput{
		InstanceProfileName: aws.String(rec.InstanceProfileName),
		TagKeys:             aws.StringSlice([]string{profileClusterTag}),
	})
	require.NoError(t, err)

	_, err = deleteNodegroupCall(f, "c1", "ng1")
	require.NoError(t, err)
	sweepNodegroups(t, f)

	require.Contains(t, m.Profiles, rec.InstanceProfileName)
	assert.Equal(t, []string{fixtureNodeRoleName}, profileRoleNames(m.Profiles[rec.InstanceProfileName]))
	assert.Zero(t, m.RemoveRoleCalls)
	_, err = describeNodegroupCall(f, "c1", "ng1")
	requireErrorCode(t, err, awserrors.ErrorEKSResourceNotFound)
}

func TestDeleteNodegroup_OldLayoutGroupDeletesAndLeavesRoleNamedProfile(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	seedCustomerProfile(f, t, fixtureNodeRoleName, fixtureNodeRoleName, nil)
	require.NoError(t, PutNodegroupRecord(t.Context(), f.kv, &NodegroupRecord{
		ClusterName: "c1", Name: "old", Status: eks.NodegroupStatusActive,
		InstanceIDs: []string{"i-old1"}, NodeRole: fixtureNodeRoleARN,
	}))

	_, err := deleteNodegroupCall(f, "c1", "old")
	require.NoError(t, err)
	sweepNodegroups(t, f)

	require.Len(t, f.worker.terminateCalls, 1)
	assert.Equal(t, []string{"i-old1"}, f.worker.terminateCalls[0])
	_, err = describeNodegroupCall(f, "c1", "old")
	requireErrorCode(t, err, awserrors.ErrorEKSResourceNotFound)
	m := fixtureIAM(t, f)
	require.Contains(t, m.Profiles, fixtureNodeRoleName)
	assert.Equal(t, []string{fixtureNodeRoleName}, profileRoleNames(m.Profiles[fixtureNodeRoleName]))
	assert.Zero(t, m.RemoveRoleCalls)
	assert.Zero(t, m.DeleteInstanceProfileCalls)
}

// No path deletes a profile unless it is recorded, tagged for this
// incarnation, holds the record's node role and is named by no other record.
func TestInvariant_NodegroupDeleteNeverRemovesAProfileItDoesNotOwn(t *testing.T) {
	t.Parallel()
	ownership := map[string]string{profileClusterTag: "c1", profileNodegroupTag: "ng1"}

	t.Run("customer pre-creates a profile at a guessed name", func(t *testing.T) {
		t.Parallel()
		f := newEKSServiceFixture(t)
		seedActiveClusterWithToken(t, f, "c1")
		guesses := []string{
			"eks-" + arn.EKSNodegroupDiscriminator(testAccountID, "c1", "ng1"),
			"eks-c1-ng1",
			"c1-ng1",
			fixtureNodeRoleName,
		}
		for _, name := range guesses {
			seedCustomerProfile(f, t, name, fixtureNodeRoleName, ownership)
		}
		createReadyNodegroup(t, f, "c1", "ng1", 1)
		rec, _ := ownedProfile(t, f, "c1", "ng1")
		require.NotContains(t, guesses, rec.InstanceProfileName, "the name must not be guessable")

		_, err := deleteNodegroupCall(f, "c1", "ng1")
		require.NoError(t, err)
		sweepNodegroups(t, f)

		m := fixtureIAM(t, f)
		for _, name := range guesses {
			require.Contains(t, m.Profiles, name, "customer profile %q was deleted", name)
			assert.Equal(t, []string{fixtureNodeRoleName}, profileRoleNames(m.Profiles[name]), "customer profile %q lost its role", name)
		}
	})

	t.Run("recorded profile was swapped for one holding another role", func(t *testing.T) {
		t.Parallel()
		f := newEKSServiceFixture(t)
		seedActiveClusterWithToken(t, f, "c1")
		createReadyNodegroup(t, f, "c1", "ng1", 1)
		rec, p := ownedProfile(t, f, "c1", "ng1")
		p.Roles = []*iam.Role{{RoleName: aws.String("customer-role")}}

		_, err := deleteNodegroupCall(f, "c1", "ng1")
		require.NoError(t, err)
		sweepNodegroups(t, f)

		m := fixtureIAM(t, f)
		require.Contains(t, m.Profiles, rec.InstanceProfileName)
		assert.Equal(t, []string{"customer-role"}, profileRoleNames(m.Profiles[rec.InstanceProfileName]))
		assert.Zero(t, m.RemoveRoleCalls)
		assert.Zero(t, m.DeleteInstanceProfileCalls)
	})

	t.Run("another nodegroup record names the same profile", func(t *testing.T) {
		t.Parallel()
		f := newEKSServiceFixture(t)
		seedActiveClusterWithToken(t, f, "c1")
		createReadyNodegroup(t, f, "c1", "ng1", 1)
		rec, _ := ownedProfile(t, f, "c1", "ng1")
		require.NoError(t, PutNodegroupRecord(t.Context(), f.kv, &NodegroupRecord{
			ClusterName: "c1", Name: "ng2", Status: eks.NodegroupStatusActive, NodeRole: fixtureNodeRoleARN,
			InstanceProfileName: rec.InstanceProfileName, InstanceProfileID: rec.InstanceProfileID,
		}))

		_, err := deleteNodegroupCall(f, "c1", "ng1")
		require.NoError(t, err)
		sweepNodegroups(t, f)

		m := fixtureIAM(t, f)
		require.Contains(t, m.Profiles, rec.InstanceProfileName)
		assert.Equal(t, []string{fixtureNodeRoleName}, profileRoleNames(m.Profiles[rec.InstanceProfileName]))
		assert.Zero(t, m.DeleteInstanceProfileCalls)
	})
}

func TestDeleteNodegroup_ProfileAndRecordKeptUntilWorkersProvenGone(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	createReadyNodegroup(t, f, "c1", "ng1", 2)
	rec, _ := ownedProfile(t, f, "c1", "ng1")
	f.worker.stillLive = []string{rec.InstanceIDs[0]}
	_, err := deleteNodegroupCall(f, "c1", "ng1")
	require.NoError(t, err)

	sweepNodegroups(t, f)

	m := fixtureIAM(t, f)
	assert.Contains(t, m.Profiles, rec.InstanceProfileName)
	assert.Zero(t, m.RemoveRoleCalls)
	assert.Zero(t, m.DeleteInstanceProfileCalls)
	got, err := describeNodegroupCall(f, "c1", "ng1")
	require.NoError(t, err)
	assert.Equal(t, eks.NodegroupStatusDeleting, aws.StringValue(got.Nodegroup.Status))
	cur, err := GetNodegroupRecord(t.Context(), f.kv, "c1", "ng1")
	require.NoError(t, err)
	assert.Equal(t, rec.InstanceIDs, cur.InstanceIDs, "worker ids stay recorded until they are proven gone")
	assert.Zero(t, cur.DeleteReapAttempts, "workers still ending is not a failed pass")

	f.worker.mu.Lock()
	f.worker.stillLive = nil
	f.worker.mu.Unlock()
	sweepNodegroups(t, f)
	_, err = describeNodegroupCall(f, "c1", "ng1")
	requireErrorCode(t, err, awserrors.ErrorEKSResourceNotFound)
	assert.NotContains(t, m.Profiles, rec.InstanceProfileName)
}

func TestDeleteNodegroup_UndecidableWorkerStateKeepsProfile(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	createReadyNodegroup(t, f, "c1", "ng1", 1)
	rec, _ := ownedProfile(t, f, "c1", "ng1")
	f.worker.liveErr = errors.New("describe fan-out did not complete")
	_, err := deleteNodegroupCall(f, "c1", "ng1")
	require.NoError(t, err)

	sweepNodegroups(t, f)

	m := fixtureIAM(t, f)
	assert.Contains(t, m.Profiles, rec.InstanceProfileName)
	assert.Zero(t, m.DeleteInstanceProfileCalls)
}

func TestDeleteNodegroup_KickFinishesTheDeleteWithoutAReaperSweep(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	createReadyNodegroup(t, f, "c1", "ng1", 1)
	rec, _ := ownedProfile(t, f, "c1", "ng1")
	f.svc.noDeleteKick = false

	_, err := deleteNodegroupCall(f, "c1", "ng1")
	require.NoError(t, err)
	f.svc.WaitNodegroupDeletes()

	_, err = describeNodegroupCall(f, "c1", "ng1")
	requireErrorCode(t, err, awserrors.ErrorEKSResourceNotFound)
	assert.NotContains(t, fixtureIAM(t, f).Profiles, rec.InstanceProfileName)
}
