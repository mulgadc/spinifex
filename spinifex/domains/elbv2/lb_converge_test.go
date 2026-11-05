package elbv2

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLBRepair_AnotherReservationWinsTheFirstCAS(t *testing.T) {
	t.Parallel()
	e := newRepairEnv(t)
	lb := e.seed("lbr20", true)
	cands := e.candidates()
	require.Len(t, cands, 1)
	require.True(t, e.rec.eligible(context.Background(), cands[0], e.clock))

	_, err := e.svc.store.UpdateLBHealth(context.Background(), lb.LoadBalancerID, func(rec *LBDataPathRecord) (bool, error) {
		rec.Repairs = append(rec.Repairs, LBRepair{At: e.clock, InstanceID: lb.InstanceID, Outcome: RepairReserved})
		return true, nil
	})
	require.NoError(t, err)

	assert.False(t, e.rec.repair(context.Background(), cands[0]))
	assert.Empty(t, e.reboots())
	assert.Len(t, e.health(lb.LoadBalancerID).Repairs, 1, "the losing reservation must not add an entry")
}

func TestLBRepair_LeaseLostAfterReservationKeepsItCountedAndDoesNotDispatch(t *testing.T) {
	t.Parallel()
	e := newRepairEnv(t)
	e.seed("lbr21", true)
	cands := e.candidates()
	require.Len(t, cands, 1)

	calls := 0
	e.rec.isLeader = func() bool {
		calls++
		return calls == 1
	}
	assert.True(t, e.rec.repair(context.Background(), cands[0]), "a written reservation is reported as spent")
	assert.Empty(t, e.reboots())

	repairs := e.health("lbr21").Repairs
	require.Len(t, repairs, 1)
	assert.Equal(t, RepairReserved, repairs[0].Outcome, "the entry stays counted with no outcome")
}

func TestLBRepair_NonLeaderNeverReserves(t *testing.T) {
	t.Parallel()
	e := newRepairEnv(t)
	e.seed("lbr22", true)
	cands := e.candidates()
	require.Len(t, cands, 1)

	e.rec.isLeader = func() bool { return false }
	assert.False(t, e.rec.repair(context.Background(), cands[0]))
	assert.Empty(t, e.health("lbr22").Repairs)
}

func TestLBHealth_FailedResumeThenNewVMHeartbeatsConverge(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lb := seedHealthLB(t, svc)
	ctx := context.Background()

	// The old VM's evidence was suspended by SetSubnets, whose resume write then failed.
	_, err := svc.store.UpdateLBHealth(ctx, lb.LoadBalancerID, func(rec *LBDataPathRecord) (bool, error) {
		rec.InstanceID = "i-oldvm"
		rec.Suspended = true
		rec.SuspendedAt = time.Now().UTC()
		return true, nil
	})
	require.NoError(t, err)

	healthBeat(t, svc, aws.String(healthInst), nicStatus(healthMACData, true, 0, 10))
	rec, err := svc.store.GetLBHealth(ctx, lb.LoadBalancerID)
	require.NoError(t, err)
	assert.False(t, rec.Suspended, "a heartbeat from the VM the LB names lifts the stale suspension")
	assert.Equal(t, healthInst, rec.InstanceID)

	healthBeat(t, svc, aws.String(healthInst), nicStatus(healthMACData, false, 3, 11))
	rec, err = svc.store.GetLBHealth(ctx, lb.LoadBalancerID)
	require.NoError(t, err)
	require.Contains(t, rec.NICs, healthMACData, "a later NIC failure is recorded and repairable")
}

func TestLBHealth_SuspensionIsNotLiftedByAHeartbeatFromAVMTheLBDoesNotName(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lb := seedHealthLB(t, svc)
	ctx := context.Background()

	_, err := svc.store.UpdateLBHealth(ctx, lb.LoadBalancerID, func(rec *LBDataPathRecord) (bool, error) {
		rec.InstanceID = "i-oldvm"
		rec.Suspended = true
		rec.SuspendedAt = time.Now().UTC()
		return true, nil
	})
	require.NoError(t, err)

	healthBeat(t, svc, aws.String("i-oldvm"), nicStatus(healthMACData, false, 3, 1))
	rec, err := svc.store.GetLBHealth(ctx, lb.LoadBalancerID)
	require.NoError(t, err)
	assert.True(t, rec.Suspended)
	assert.Equal(t, "i-oldvm", rec.InstanceID)
}

func TestLBHealth_SuspensionBackstopLiftsOnlyAfterThirtyMinutes(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lb := seedHealthLB(t, svc)
	ctx := context.Background()

	suspend := func(age time.Duration) {
		_, err := svc.store.UpdateLBHealth(ctx, lb.LoadBalancerID, func(rec *LBDataPathRecord) (bool, error) {
			rec.InstanceID = healthInst
			rec.Suspended = true
			rec.SuspendedAt = time.Now().UTC().Add(-age)
			return true, nil
		})
		require.NoError(t, err)
	}

	suspend(29 * time.Minute)
	healthBeat(t, svc, aws.String(healthInst), nicStatus(healthMACData, false, 3, 1))
	rec, err := svc.store.GetLBHealth(ctx, lb.LoadBalancerID)
	require.NoError(t, err)
	assert.True(t, rec.Suspended, "inside the backstop the suspension holds")

	suspend(31 * time.Minute)
	healthBeat(t, svc, aws.String(healthInst), nicStatus(healthMACData, false, 3, 2))
	rec, err = svc.store.GetLBHealth(ctx, lb.LoadBalancerID)
	require.NoError(t, err)
	assert.False(t, rec.Suspended, "past the backstop the current VM's heartbeat lifts it")
	assert.Contains(t, rec.NICs, healthMACData)
}

func TestLBHealth_EstablishedFailureSurvivesAnAgentRestart(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lb := seedHealthLB(t, svc)
	ctx := context.Background()

	healthBeat(t, svc, aws.String(healthInst), nicStatus(healthMACData, false, 3, 1))
	first, err := svc.store.GetLBHealth(ctx, lb.LoadBalancerID)
	require.NoError(t, err)
	since := first.NICs[healthMACData].FailingSince

	// A restarted agent reports healthy until it has missed three probes again.
	healthBeat(t, svc, aws.String(healthInst), nicStatus(healthMACData, true, 1, 2))
	healthBeat(t, svc, aws.String(healthInst), nicStatus(healthMACData, true, 2, 3))
	rec, err := svc.store.GetLBHealth(ctx, lb.LoadBalancerID)
	require.NoError(t, err)
	require.Contains(t, rec.NICs, healthMACData)
	assert.True(t, since.Equal(rec.NICs[healthMACData].FailingSince), "FailingSince is kept")

	healthBeat(t, svc, aws.String(healthInst), nicStatus(healthMACData, true, 0, 4))
	rec, err = svc.store.GetLBHealth(ctx, lb.LoadBalancerID)
	require.NoError(t, err)
	assert.Empty(t, rec.NICs, "only a clean probe clears the failure")
}

func TestLBHealth_OldVMHeartbeatPausedAcrossPublicationLeavesTheNewBinding(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lb := seedHealthLB(t, svc)
	ctx := context.Background()

	// SetSubnets has already published the new VM and rebound the evidence.
	lb.InstanceID = "i-newvm"
	require.NoError(t, svc.store.PutLoadBalancer(ctx, lb))
	require.NoError(t, svc.store.ResumeLBHealth(ctx, lb.LoadBalancerID, "i-newvm"))

	// The old VM's heartbeat read the LB before that and runs now.
	stale := *lb
	stale.InstanceID = healthInst
	svc.recordDataPathEvidence(ctx, &stale, &LBAgentHeartbeatInput{
		LBID:       aws.String(lb.LoadBalancerID),
		InstanceID: aws.String(healthInst),
		NICs:       []*LBAgentNICStatus{nicStatus(healthMACData, false, 5, 1)},
	})

	rec, err := svc.store.GetLBHealth(ctx, lb.LoadBalancerID)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, "i-newvm", rec.InstanceID)
	assert.Empty(t, rec.NICs, "the old VM's failure must not land on the new binding")
}
