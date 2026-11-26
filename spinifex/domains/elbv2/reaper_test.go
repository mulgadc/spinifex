package elbv2

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/nats-io/nats.go/jetstream"
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
	t.Parallel()
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
	assert.Equal(t, provisioningTimeoutReason(), got.StateReason)

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
	t.Parallel()
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
	}, 3*time.Second, 10*time.Millisecond, "reaper goroutine should mark the stuck LB failed")

	cancel()
}

func provisioningTimeoutReason() string {
	return fmt.Sprintf("lb-agent did not heartbeat within %s of provisioning", lbProvisioningTimeout)
}

// A timed-out appliance stays failed with its VM, so only an explicit delete
// reclaims it. Verifies: ADR-0003:S7
func TestReapStuckProvisioningLBsRetainsTheFailedLBsVM(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	launcher := &mockSystemInstanceLauncher{}
	svc.InstanceLauncher = launcher
	now := time.Now().UTC()

	stuck := testLBRecord("stuck0000000000", StateProvisioning, now.Add(-10*time.Minute), time.Time{})
	stuck.InstanceID = "i-stuck"
	noVM := testLBRecord("novm0000000000", StateProvisioning, now.Add(-10*time.Minute), time.Time{})
	fresh := testLBRecord("fresh0000000000", StateProvisioning, now, time.Time{})
	fresh.InstanceID = "i-fresh"
	active := testLBRecord("active000000000", StateActive, now.Add(-10*time.Minute), time.Time{})
	active.InstanceID = "i-active"
	heartbeating := testLBRecord("heartbeat0000000", StateProvisioning, now.Add(-10*time.Minute), now.Add(-time.Second))
	heartbeating.InstanceID = "i-heartbeating"
	for _, lb := range []*LoadBalancerRecord{stuck, noVM, fresh, active, heartbeating} {
		require.NoError(t, svc.store.PutLoadBalancer(t.Context(), lb))
	}

	svc.reapStuckProvisioningLBs(t.Context(), now)

	got, err := svc.store.GetLoadBalancer(t.Context(), stuck.LoadBalancerID)
	require.NoError(t, err)
	assert.Equal(t, StateFailed, got.State)
	assert.Equal(t, provisioningTimeoutReason(), got.StateReason, "the failure reason stays on the LB")
	assert.Equal(t, "i-stuck", got.InstanceID, "the failed LB keeps its VM reference")

	got, err = svc.store.GetLoadBalancer(t.Context(), noVM.LoadBalancerID)
	require.NoError(t, err)
	assert.Equal(t, StateFailed, got.State)
	for _, lb := range []*LoadBalancerRecord{fresh, active, heartbeating} {
		got, err := svc.store.GetLoadBalancer(t.Context(), lb.LoadBalancerID)
		require.NoError(t, err)
		assert.Equal(t, lb.State, got.State, "lb %s", lb.LoadBalancerID)
		assert.Equal(t, lb.InstanceID, got.InstanceID, "lb %s", lb.LoadBalancerID)
	}

	launcher.mu.Lock()
	defer launcher.mu.Unlock()
	assert.Empty(t, launcher.terminateCalls, "the timeout must not terminate any VM")
}

// Verifies: ADR-0003:S7
func TestReapStuckProvisioningLBsKeepsTheFailedLBsENIAttached(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		terminateErr error
	}{
		{"a VM that could be stopped keeps its ENI", nil},
		{"a VM that cannot be stopped keeps its ENI", errors.New("nats timeout")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc, vpcSvc, subnetID := setupMicrovmTestService(t)
			launcher := &mockSystemInstanceLauncher{terminateErr: tc.terminateErr}
			svc.InstanceLauncher = launcher

			eniOut, err := vpcSvc.CreateNetworkInterface(t.Context(), &ec2.CreateNetworkInterfaceInput{SubnetId: aws.String(subnetID)}, testAccountID)
			require.NoError(t, err)
			eniID := aws.StringValue(eniOut.NetworkInterface.NetworkInterfaceId)
			_, err = vpcSvc.AttachENI(testAccountID, eniID, "i-stuck", 0)
			require.NoError(t, err)

			now := time.Now().UTC()
			lb := testLBRecord("enirel0000000000", StateProvisioning, now.Add(-10*time.Minute), time.Time{})
			lb.AccountID = testAccountID
			lb.InstanceID = "i-stuck"
			lb.ENIs = []string{eniID}
			require.NoError(t, svc.store.PutLoadBalancer(t.Context(), lb))

			svc.reapStuckProvisioningLBs(t.Context(), now)

			got, err := svc.store.GetLoadBalancer(t.Context(), lb.LoadBalancerID)
			require.NoError(t, err)
			assert.Equal(t, StateFailed, got.State)
			assert.Equal(t, provisioningTimeoutReason(), got.StateReason)
			assert.Equal(t, "i-stuck", got.InstanceID)
			assert.Equal(t, []string{eniID}, got.ENIs)

			rec, err := vpcSvc.GetENIRecord(testAccountID, eniID)
			require.NoError(t, err, "the ENI stays for DeleteLoadBalancer to remove")
			assert.Equal(t, "in-use", rec.Status)
			assert.Equal(t, "i-stuck", rec.InstanceId)

			launcher.mu.Lock()
			defer launcher.mu.Unlock()
			assert.Empty(t, launcher.terminateCalls)
		})
	}
}

// The appliance timeout owns only records with no realization.
func TestReapStuckProvisioningLBsLeavesOtherRealizations(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	launcher := &mockSystemInstanceLauncher{}
	svc.InstanceLauncher = launcher
	now := time.Now().UTC()

	ovn := testLBRecord("ovnaged00000000", StateProvisioning, now.Add(-10*time.Minute), time.Time{})
	ovn.Type = LoadBalancerTypeNetwork
	ovn.Realization = "ovn"
	require.NoError(t, svc.store.PutLoadBalancer(t.Context(), ovn))
	before, err := svc.store.GetLoadBalancer(t.Context(), ovn.LoadBalancerID)
	require.NoError(t, err)

	svc.reapStuckProvisioningLBs(t.Context(), now)

	got, err := svc.store.GetLoadBalancer(t.Context(), ovn.LoadBalancerID)
	require.NoError(t, err)
	assert.Equal(t, before, got, "an aged ovn record stays unchanged")
	launcher.mu.Lock()
	defer launcher.mu.Unlock()
	assert.Empty(t, launcher.terminateCalls)
}

// writeHookKV runs hook once, just before the first Put or Update of trigger.
type writeHookKV struct {
	jetstream.KeyValue

	fired   atomic.Bool
	trigger string
	hook    func()
}

func (k *writeHookKV) before(key string) {
	if key == k.trigger && k.fired.CompareAndSwap(false, true) {
		k.hook()
	}
}

func (k *writeHookKV) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	k.before(key)
	return k.KeyValue.Put(ctx, key, value)
}

func (k *writeHookKV) Update(ctx context.Context, key string, value []byte, last uint64) (uint64, error) {
	k.before(key)
	return k.KeyValue.Update(ctx, key, value, last)
}

// A heartbeat that commits between the reaper's read and its write wins.
func TestReapStuckProvisioningLBsKeepsAWinningHeartbeat(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	launcher := &mockSystemInstanceLauncher{}
	svc.InstanceLauncher = launcher
	now := time.Now().UTC()

	lb := testLBRecord("racehb000000000", StateProvisioning, now.Add(-10*time.Minute), time.Time{})
	lb.InstanceID = "i-race"
	require.NoError(t, svc.store.PutLoadBalancer(t.Context(), lb))

	inner := svc.store.kv
	var heartbeatErr error
	var committed *LoadBalancerRecord
	gate := &writeHookKV{KeyValue: inner, trigger: KeyPrefixLB + lb.LoadBalancerID}
	gate.hook = func() {
		_, heartbeatErr = svc.LBAgentHeartbeat(t.Context(), &LBAgentHeartbeatInput{
			LBID: aws.String(lb.LoadBalancerID), InstanceID: aws.String(lb.InstanceID),
		}, testAccountID)
		committed, _ = svc.store.GetLoadBalancer(t.Context(), lb.LoadBalancerID)
	}
	svc.store.kv = gate

	svc.reapStuckProvisioningLBs(t.Context(), now)
	svc.store.kv = inner

	require.True(t, gate.fired.Load(), "the reaper must reach its write")
	require.NoError(t, heartbeatErr)
	require.NotNil(t, committed)
	require.Equal(t, StateActive, committed.State, "the heartbeat commits before the reaper writes")
	got, err := svc.store.GetLoadBalancer(t.Context(), lb.LoadBalancerID)
	require.NoError(t, err)
	assert.Equal(t, StateActive, got.State, "the committed heartbeat activation survives")
	assert.Empty(t, got.StateReason)
	assert.False(t, got.LastHeartbeat.IsZero(), "the committed heartbeat time survives")
	assert.Equal(t, "i-race", got.InstanceID)
	launcher.mu.Lock()
	defer launcher.mu.Unlock()
	assert.Empty(t, launcher.terminateCalls)
}
