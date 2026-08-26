// Leader election, the converge pass and the instance-role document are all
// unexported, and the pass is only reachable through the scheduler's own state.
//
//test:in-package — the converge pass has no exported entry point
package handlers_ecs

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/testutil"
)

// TestScheduler_StandsDownWithoutIAM is the fleet outage this guards: a leader
// that cannot converge instance roles holds the lease, writes no sts:AssumeRole
// grant, and every agent's credentials lapse an hour later.
func TestScheduler_StandsDownWithoutIAM(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	svc := NewService(nc, testRegion, "")
	sc := NewScheduler(nc, svc, "holder-a")
	require.NoError(t, sc.leaseErr)

	assert.False(t, sc.lease.TryAcquire(t.Context()), "a node without IAM must refuse the lease")
	assert.False(t, sc.lease.Held())
	assert.Zero(t, sc.subCount())

	// The key must be free, or a healthy node waits out the full TTL.
	kv, err := InitLeaderBucket(t.Context(), js)
	require.NoError(t, err)
	_, err = kv.Get(t.Context(), schedulerLeaderKey)
	require.Error(t, err)
}

// TestConvergeInstanceRoles_RewritesProvisionedAccounts pins the pass that
// carries the grant to already-running clusters, which provisioning alone
// never reaches once capacity has stopped changing.
func TestConvergeInstanceRoles_RewritesProvisionedAccounts(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	iam := newStubIAM()
	svc := NewService(nc, testRegion, "").WithDeps(Deps{IAM: iam})
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	_, err = svc.ensureECSInstanceProfile(testAccountID)
	require.NoError(t, err)

	iam.RolePolicies[ecsInstanceRoleName] = `{"Version":"2012-10-17","Statement":[]}`
	sc := NewScheduler(nc, svc, "holder-a")
	require.NoError(t, sc.convergeInstanceRoles(t.Context()))

	assert.Contains(t, iam.RolePolicies[ecsInstanceRoleName], "sts:AssumeRole")
}

// TestConvergeInstanceRoles_SkipsAccountsWithoutTheRole keeps the pass from
// creating customer-visible IAM entities in an account that only ever created
// a cluster: the bucket exists there, but nothing draws credentials.
func TestConvergeInstanceRoles_SkipsAccountsWithoutTheRole(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	iam := newStubIAM()
	svc := NewService(nc, testRegion, "").WithDeps(Deps{IAM: iam})
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)

	sc := NewScheduler(nc, svc, "holder-a")
	require.NoError(t, sc.convergeInstanceRoles(t.Context()))

	assert.Zero(t, iam.CreateRoleCalls)
	assert.Zero(t, iam.CreateInstanceProfileCalls)
	assert.Empty(t, iam.PolicyCalls)
}

// TestConvergeInstanceRoles_NilIAMIsAnError keeps the guard fail-closed for a
// caller that reaches the pass without going through leader election.
func TestConvergeInstanceRoles_NilIAMIsAnError(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	sc := NewScheduler(nc, NewService(nc, testRegion, ""), "holder-a")

	err := sc.convergeInstanceRoles(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "IAM", "the log must name the missing dependency")
}

// TestECSInstanceRolePolicyDoc_GrantsAssumeRole is the identity-side half of the
// two-gate model for the agent: without it every task-role and execution-role
// credential mint is denied about an hour after an upgrade.
func TestECSInstanceRolePolicyDoc_GrantsAssumeRole(t *testing.T) {
	doc := ecsInstanceRolePolicyDoc(testAccountID)

	assert.Contains(t, doc, `{"Effect":"Allow","Action":"sts:AssumeRole","Resource":"arn:aws:iam::`+testAccountID+`:role/*"}`)
	assert.Contains(t, doc, `"ecs:*"`)
	assert.Contains(t, doc, `"ecr:GetAuthorizationToken"`)
}
