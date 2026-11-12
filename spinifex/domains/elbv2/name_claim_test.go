package elbv2

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Two concurrent identical CreateLoadBalancer calls create one load balancer.
// A loser that finds the winner's record gets it back; one that finds only the
// pending claim gets DuplicateLoadBalancerName. Under -race a double-claim would surface.
func TestCreateLoadBalancer_ConcurrentSameNameSingleOwner(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)

	const n = 2
	outs := make([]*elbv2.CreateLoadBalancerOutput, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outs[i], errs[i] = svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
				Name: aws.String("race-lb"),
			}, testAccountID)
		}(i)
	}
	wg.Wait()

	rec, err := svc.store.GetLoadBalancerByName(t.Context(), "race-lb", testAccountID)
	require.NoError(t, err)
	require.NotNil(t, rec)

	var ok int
	for i, e := range errs {
		if e == nil {
			ok++
			assert.Equal(t, rec.LoadBalancerArn, *outs[i].LoadBalancers[0].LoadBalancerArn, "every success names the one load balancer")
			continue
		}
		code, _, _ := awserrors.ResolveErrorDetail(e)
		require.Equal(t, awserrors.ErrorELBv2DuplicateLoadBalancer, code, "unexpected create error: %v", e)
	}
	assert.GreaterOrEqual(t, ok, 1, "one create owns the name")

	desc, err := svc.DescribeLoadBalancers(context.Background(), &elbv2.DescribeLoadBalancersInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.LoadBalancers, 1)
}

// TestClaimLBName_PendingClaimBlocksSecondClaimer isolates the orphan-reclaim
// window: a claim with no persisted LB record yet — the gap a real create
// leaves open — must not be stealable by a second claimer within the TTL.
func TestClaimLBName_PendingClaimBlocksSecondClaimer(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)

	ok1, dup1, err := svc.store.ClaimLBName(t.Context(), "pending-lb", testAccountID, "lb-first")
	require.NoError(t, err)
	require.True(t, ok1)
	require.False(t, dup1)

	// lb-first has not persisted a record yet — the second claimer must not
	// mistake that for a crash orphan.
	ok2, dup2, err := svc.store.ClaimLBName(t.Context(), "pending-lb", testAccountID, "lb-second")
	require.NoError(t, err)
	assert.False(t, ok2, "a pending claim within its TTL must not be stealable")
	assert.True(t, dup2, "the second claimer must see the name as unavailable")
}

// A name claim whose owner resolves to no live record, past its TTL, is a
// crashed prior create. A fresh CreateLoadBalancer must reclaim it and succeed.
// The injectable clock ages the claim so the test never sleeps.
func TestCreateLoadBalancer_ReclaimsCrashOrphanNameClaim(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)

	// Seed a name claim pointing at a non-existent LB (crashed create left no
	// record), stamped as claimed well before the TTL via a fake clock.
	realNow := svc.store.now
	svc.store.now = func() time.Time { return realNow().Add(-2 * lbNameClaimTTL) }
	ok, dup, err := svc.store.ClaimLBName(t.Context(), "orphan-lb", testAccountID, "lb-doesnotexist")
	require.NoError(t, err)
	require.True(t, ok)
	require.False(t, dup)
	svc.store.now = realNow

	// A fresh create for the same name reclaims the orphan and succeeds.
	out, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name: aws.String("orphan-lb"),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.LoadBalancers, 1)
}

// Deleting a load balancer releases its name claim so the name is immediately
// reusable.
func TestDeleteLoadBalancer_ReleasesNameForReuse(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)

	out, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name: aws.String("reuse-lb"),
	}, testAccountID)
	require.NoError(t, err)
	arn := out.LoadBalancers[0].LoadBalancerArn

	_, err = svc.DeleteLoadBalancer(context.Background(), &elbv2.DeleteLoadBalancerInput{
		LoadBalancerArn: arn,
	}, testAccountID)
	require.NoError(t, err)

	// The name is free: a second create with the same name succeeds.
	_, err = svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name: aws.String("reuse-lb"),
	}, testAccountID)
	require.NoError(t, err)
}

// A create that claims the name but then fails before PutLoadBalancer (here, ENI
// provisioning on a nonexistent subnet) must release the claim on its way out, so
// a retry with the same name is not permanently locked out by the failed attempt.
func TestCreateLoadBalancer_FailureAfterClaimReleasesNameForRetry(t *testing.T) {
	t.Parallel()
	svc, _ := setupTestServiceWithVPC(t)

	_, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("retry-lb"),
		Subnets: []*string{aws.String("subnet-doesnotexist")},
	}, testAccountID)
	require.Error(t, err, "ENI creation on a nonexistent subnet must fail")

	// The failed attempt released the claim: a retry with no subnets (nothing
	// left to fail) succeeds under the same name.
	out, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name: aws.String("retry-lb"),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.LoadBalancers, 1)
}
