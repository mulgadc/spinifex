package handlers_elbv2

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testLBRecord builds a minimal, valid LoadBalancerRecord for reaper tests.
func testLBRecord(id, state string, createdAt, lastHeartbeat time.Time) *LoadBalancerRecord {
	return &LoadBalancerRecord{
		LoadBalancerArn: "arn:aws:elasticloadbalancing:us-east-1:" + testAccountID + ":loadbalancer/app/reaper-" + id + "/" + id,
		LoadBalancerID:  id,
		Name:            "reaper-" + id,
		State:           state,
		AccountID:       testAccountID,
		CreatedAt:       createdAt,
		LastHeartbeat:   lastHeartbeat,
	}
}

func TestReapStuckProvisioningLBs(t *testing.T) {
	svc := setupTestService(t)
	now := time.Now().UTC()

	stuck := testLBRecord("stuck0000000000", StateProvisioning, now.Add(-10*time.Minute), time.Time{})
	require.NoError(t, svc.store.PutLoadBalancer(t.Context(), stuck))

	fresh := testLBRecord("fresh0000000000", StateProvisioning, now, time.Time{})
	require.NoError(t, svc.store.PutLoadBalancer(t.Context(), fresh))

	activeOld := testLBRecord("active000000000", StateActive, now.Add(-10*time.Minute), time.Time{})
	require.NoError(t, svc.store.PutLoadBalancer(t.Context(), activeOld))

	heartbeating := testLBRecord("heartbeat0000000", StateProvisioning, now.Add(-10*time.Minute), now.Add(-time.Second))
	require.NoError(t, svc.store.PutLoadBalancer(t.Context(), heartbeating))

	svc.reapStuckProvisioningLBs(t.Context(), now)

	got, err := svc.store.GetLoadBalancer(t.Context(), stuck.LoadBalancerID)
	require.NoError(t, err)
	assert.Equal(t, StateFailed, got.State)
	assert.NotEmpty(t, got.StateReason)

	got, err = svc.store.GetLoadBalancer(t.Context(), fresh.LoadBalancerID)
	require.NoError(t, err)
	assert.Equal(t, StateProvisioning, got.State)

	got, err = svc.store.GetLoadBalancer(t.Context(), activeOld.LoadBalancerID)
	require.NoError(t, err)
	assert.Equal(t, StateActive, got.State)

	got, err = svc.store.GetLoadBalancer(t.Context(), heartbeating.LoadBalancerID)
	require.NoError(t, err)
	assert.Equal(t, StateProvisioning, got.State)
}

// TestStartLifecycleReaper drives the ticker goroutine with a tiny interval and
// confirms it reaps a stuck LB, then cancels to exercise the ctx.Done exit.
func TestStartLifecycleReaper(t *testing.T) {
	svc := setupTestService(t)
	svc.reaperInterval = 5 * time.Millisecond

	stuck := testLBRecord("ticker0000000000", StateProvisioning, time.Now().UTC().Add(-10*time.Minute), time.Time{})
	require.NoError(t, svc.store.PutLoadBalancer(t.Context(), stuck))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.StartLifecycleReaper(ctx)

	require.Eventually(t, func() bool {
		got, err := svc.store.GetLoadBalancer(t.Context(), stuck.LoadBalancerID)
		return err == nil && got != nil && got.State == StateFailed
	}, time.Second, 10*time.Millisecond, "reaper goroutine should mark the stuck LB failed")

	cancel()
}
