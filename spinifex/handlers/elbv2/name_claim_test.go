package handlers_elbv2

import (
	"context"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Two concurrent CreateLoadBalancer calls for the same name resolve to exactly
// one success and one DuplicateLoadBalancerName. The atomic name claim is the
// barrier; under -race a double-claim would surface as a launcher/store race.
func TestCreateLoadBalancer_ConcurrentSameNameSingleOwner(t *testing.T) {
	svc := setupTestService(t)

	const n = 2
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
				Name: aws.String("race-lb"),
			}, testAccountID)
		}(i)
	}
	wg.Wait()

	var ok, dup int
	for _, e := range errs {
		switch {
		case e == nil:
			ok++
		case e.Error() == "DuplicateLoadBalancerName":
			dup++
		default:
			t.Fatalf("unexpected create error: %v", e)
		}
	}
	assert.Equal(t, 1, ok, "exactly one create owns the name")
	assert.Equal(t, 1, dup, "the duplicate is rejected DuplicateLoadBalancerName")

	rec, err := svc.store.GetLoadBalancerByName("race-lb", testAccountID)
	require.NoError(t, err)
	require.NotNil(t, rec)
}

// A name claim whose owner lbID resolves to no live record is a crashed prior
// create. A fresh CreateLoadBalancer with that name must reclaim the orphan and
// succeed, not wedge on DuplicateLoadBalancerName forever.
func TestCreateLoadBalancer_ReclaimsCrashOrphanNameClaim(t *testing.T) {
	svc := setupTestService(t)

	// Seed a name claim pointing at a non-existent LB (crashed create left no
	// record). ClaimLBName writes the key with this bogus owner.
	ok, dup, err := svc.store.ClaimLBName("orphan-lb", testAccountID, "lb-doesnotexist")
	require.NoError(t, err)
	require.True(t, ok)
	require.False(t, dup)

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
