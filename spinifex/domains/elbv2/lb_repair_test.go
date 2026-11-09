package elbv2

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/systeminstance"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const repairMAC = "02:00:00:bb:00:01"

type repairEnv struct {
	t        *testing.T
	svc      *ELBv2ServiceImpl
	rec      *lbRepairReconciler
	launcher *mockSystemInstanceLauncher
	clock    time.Time
	leader   bool
}

func newRepairEnv(t *testing.T) *repairEnv {
	t.Helper()
	e := &repairEnv{
		t:        t,
		svc:      setupTestService(t),
		launcher: &mockSystemInstanceLauncher{},
		clock:    time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC),
		leader:   true,
	}
	e.rec = newLBRepairReconciler(e.svc, e.launcher, func() bool { return e.leader })
	e.rec.now = func() time.Time { return e.clock }
	return e
}

// seed stores an active LB whose VM has run for ten minutes and whose data NIC
// has been failing for two, with a heartbeat from this instant.
func (e *repairEnv) seed(id string, failing bool) *LoadBalancerRecord {
	e.t.Helper()
	lb := &LoadBalancerRecord{
		LoadBalancerArn: "arn:aws:elasticloadbalancing:us-east-1:" + testAccountID + ":loadbalancer/app/" + id + "/" + id,
		LoadBalancerID:  id,
		Name:            id,
		State:           StateActive,
		InstanceID:      "i-" + id,
		AccountID:       testAccountID,
		NodeID:          "node-1",
		CreatedAt:       e.clock.Add(-time.Hour),
		ActiveSince:     e.clock.Add(-10 * time.Minute),
		LastHeartbeat:   e.clock,
	}
	require.NoError(e.t, e.svc.store.PutLoadBalancer(context.Background(), lb))
	if failing {
		e.fail(id, 2*time.Minute)
	}
	return lb
}

func (e *repairEnv) fail(id string, age time.Duration) {
	e.t.Helper()
	_, err := e.svc.store.UpdateLoadBalancer(context.Background(), id, func(lb *LoadBalancerRecord) (bool, error) {
		lb.LastHeartbeat = e.clock
		return true, nil
	})
	require.NoError(e.t, err)
	_, err = e.svc.store.UpdateLBHealth(context.Background(), id, func(rec *LBDataPathRecord) (bool, error) {
		rec.InstanceID = "i-" + id
		rec.NICs = map[string]*LBNICFailure{
			repairMAC: {ENIID: "eni-" + id, FailingSince: e.clock.Add(-age), ConsecutiveFailures: 9, RxPackets: 4, TxPackets: 8},
		}
		return true, nil
	})
	require.NoError(e.t, err)
}

func (e *repairEnv) health(id string) *LBDataPathRecord {
	e.t.Helper()
	rec, err := e.svc.store.GetLBHealth(context.Background(), id)
	require.NoError(e.t, err)
	require.NotNil(e.t, rec)
	return rec
}

func (e *repairEnv) pass() { e.rec.pass(context.Background()) }

func (e *repairEnv) reboots() []string {
	e.launcher.mu.Lock()
	defer e.launcher.mu.Unlock()
	return append([]string(nil), e.launcher.rebootCalls...)
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestLBRepair_RepairsOnlyAfterSixtySecondsOfFailure(t *testing.T) {
	t.Parallel()
	e := newRepairEnv(t)
	e.seed("lbr1", false)
	e.fail("lbr1", 59*time.Second)
	e.pass()
	assert.Empty(t, e.reboots())

	e.clock = e.clock.Add(2 * time.Second)
	e.pass()
	assert.Equal(t, []string{"i-lbr1"}, e.reboots())
	h := e.health("lbr1")
	require.Len(t, h.Repairs, 1)
	assert.Equal(t, RepairDispatched, h.Repairs[0].Outcome)
	assert.Equal(t, "eni-lbr1", h.Repairs[0].ENIID)
}

func TestLBRepair_ContinuouslyHeartbeatingLBIsNeverStale(t *testing.T) {
	t.Parallel()
	// A healthy agent persists its heartbeat only every minute, so the stored
	// timestamp can be any age below that plus one pass at the reconciler.
	for age := time.Duration(0); age < lbRepairHeartbeatMaxAge; age += 15 * time.Second {
		e := newRepairEnv(t)
		lb := e.seed("lbr2", true)
		lb.LastHeartbeat = e.clock.Add(-age)
		require.NoError(t, e.svc.store.PutLoadBalancer(context.Background(), lb))
		e.pass()
		assert.Len(t, e.reboots(), 1, "heartbeat %s old", age)
	}

	e := newRepairEnv(t)
	lb := e.seed("lbr2", true)
	lb.LastHeartbeat = e.clock.Add(-lbRepairHeartbeatMaxAge)
	require.NoError(t, e.svc.store.PutLoadBalancer(context.Background(), lb))
	e.pass()
	assert.Empty(t, e.reboots(), "a heartbeat older than the bound is stale")
}

func TestLBRepair_NeverWithoutCurrentEvidence(t *testing.T) {
	t.Parallel()
	mutate := map[string]func(*LoadBalancerRecord){
		"non-active LB":      func(lb *LoadBalancerRecord) { lb.State = StateProvisioning },
		"failed LB":          func(lb *LoadBalancerRecord) { lb.State = StateFailed },
		"empty instance ID":  func(lb *LoadBalancerRecord) { lb.InstanceID = "" },
		"changed instance":   func(lb *LoadBalancerRecord) { lb.InstanceID = "i-other" },
		"stale heartbeat":    func(lb *LoadBalancerRecord) { lb.LastHeartbeat = lb.LastHeartbeat.Add(-time.Hour) },
		"VM under 5 minutes": func(lb *LoadBalancerRecord) { lb.ActiveSince = lb.LastHeartbeat.Add(-4 * time.Minute) },
	}
	for name, fn := range mutate {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newRepairEnv(t)
			lb := e.seed("lbr3", true)
			fn(lb)
			require.NoError(t, e.svc.store.PutLoadBalancer(context.Background(), lb))
			e.pass()
			assert.Empty(t, e.reboots())
		})
	}
}

func TestLBRepair_NonHolderNeverRepairs(t *testing.T) {
	t.Parallel()
	e := newRepairEnv(t)
	e.seed("lbr4", true)
	e.leader = false
	e.pass()
	assert.Empty(t, e.reboots())
	e.leader = true
	e.pass()
	assert.Len(t, e.reboots(), 1)
}

// Leadership comes from the data-path lease: a node repairs nothing while
// another holds it, repairs once elected, and stops the moment it is taken over.
func TestLBRepair_ActsOnlyWhileHoldingTheDataPathLease(t *testing.T) {
	t.Parallel()
	e := newRepairEnv(t)
	e.seed("lbr4a", true)
	e.seed("lbr4b", true)
	e.fail("lbr4b", 3*time.Minute)

	store := kvstore.OpenSyncBucket(e.svc.store.js)
	lease := kvsync.NewLease(store, KeyDataPath, "node-a", 300*time.Millisecond)
	e.rec.isLeader = lease.Held
	other := NewDataPathLease(kvstore.OpenSyncBucket(e.svc.store.js), "node-b")
	tok, err := other.TryLock(t.Context())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); kvsync.RunElection(ctx, lease, nil, nil) }()
	t.Cleanup(func() { cancel(); <-done })

	e.pass()
	assert.False(t, lease.Held())
	assert.Empty(t, e.reboots(), "a node that lost the election must not repair")

	other.Unlock(t.Context(), tok)
	require.Eventually(t, lease.Held, 5*time.Second, 5*time.Millisecond)
	e.pass()
	assert.Equal(t, []string{"i-lbr4b"}, e.reboots())

	syncKV, err := store.KV(t.Context())
	require.NoError(t, err)
	_, err = syncKV.Put(t.Context(), KeyDataPath, []byte(`{"holder":"node-b","generation":0}`))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return !lease.Held() }, 5*time.Second, 5*time.Millisecond)
	e.pass()
	assert.Equal(t, []string{"i-lbr4b"}, e.reboots(), "a node whose lease was taken over must not repair")
}

func TestLBRepair_OneRepairPerPass(t *testing.T) {
	t.Parallel()
	e := newRepairEnv(t)
	e.seed("lbr5a", true)
	e.seed("lbr5b", true)
	for i := range 3 {
		e.seed(fmt.Sprintf("lbr5h%d", i), false)
	}
	e.fail("lbr5b", 3*time.Minute)

	e.pass()
	assert.Equal(t, []string{"i-lbr5b"}, e.reboots(), "the longest-failing LB goes first")
	e.pass()
	assert.Equal(t, []string{"i-lbr5b", "i-lbr5a"}, e.reboots())
}

func TestLBRepair_FleetWideFailureRepairsNothing(t *testing.T) {
	e := newRepairEnv(t)
	logs := captureLogs(t)
	e.seed("lbr6a", true)
	e.seed("lbr6b", true)
	e.seed("lbr6c", true)
	e.seed("lbr6d", false)
	e.pass()
	assert.Empty(t, e.reboots())
	assert.Contains(t, logs.String(), "ELBv2 data-path repair suppressed: fleet-wide probe failure")

	// Three failing out of seven is not more than half.
	for i := range 3 {
		e.seed(fmt.Sprintf("lbr6h%d", i), false)
	}
	e.pass()
	assert.Len(t, e.reboots(), 1)
}

func TestLBRepair_CooldownAndDailyBudget(t *testing.T) {
	e := newRepairEnv(t)
	logs := captureLogs(t)
	e.seed("lbr7", true)

	e.pass()
	require.Len(t, e.reboots(), 1)

	e.clock = e.clock.Add(9 * time.Minute)
	e.fail("lbr7", 2*time.Minute)
	e.pass()
	assert.Len(t, e.reboots(), 1, "inside the 10 minute cooldown")

	for want := 2; want <= 3; want++ {
		e.clock = e.clock.Add(11 * time.Minute)
		e.fail("lbr7", 2*time.Minute)
		e.pass()
		assert.Len(t, e.reboots(), want)
	}

	e.clock = e.clock.Add(11 * time.Minute)
	e.fail("lbr7", 2*time.Minute)
	e.pass()
	e.pass()
	assert.Len(t, e.reboots(), 3, "the fourth repair in 24 hours is refused")
	assert.Equal(t, 1, strings.Count(logs.String(), "ELBv2 data-path repair budget exhausted"))

	e.clock = e.clock.Add(24 * time.Hour)
	e.fail("lbr7", 2*time.Minute)
	e.pass()
	assert.Len(t, e.reboots(), 4, "the budget frees after 24 hours")
}

func TestLBRepair_UnreachableOwnerIsRetriedAndNotCounted(t *testing.T) {
	t.Parallel()
	e := newRepairEnv(t)
	e.launcher.rebootErr = fmt.Errorf("nats request: %w", nats.ErrNoResponders)
	e.seed("lbr8", true)
	for range 4 {
		e.pass()
		e.clock = e.clock.Add(30 * time.Second)
		e.fail("lbr8", 2*time.Minute)
	}
	assert.Len(t, e.reboots(), 4, "no node received it, so cooldown and budget do not apply")
	for _, r := range e.health("lbr8").Repairs {
		assert.Equal(t, RepairUnreachable, r.Outcome)
	}

	e.launcher.rebootErr = nil
	e.pass()
	assert.Len(t, e.reboots(), 5)
}

func TestLBRepair_RefusedAndAmbiguousRepairsStillCount(t *testing.T) {
	t.Parallel()
	for name, err := range map[string]error{
		"refused": fmt.Errorf("%w: i-lbr9", systeminstance.ErrSystemInstanceNotSystem),
		"lost":    errors.New("timeout waiting for reply"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newRepairEnv(t)
			e.launcher.rebootErr = err
			e.seed("lbr9", true)
			e.pass()
			e.clock = e.clock.Add(30 * time.Second)
			e.fail("lbr9", 2*time.Minute)
			e.pass()
			assert.Len(t, e.reboots(), 1, "inside the cooldown")
			want := RepairRefused
			if name == "lost" {
				want = RepairReserved
			}
			assert.Equal(t, want, e.health("lbr9").Repairs[0].Outcome)
		})
	}
}

func TestLBRepair_ReservationWithoutOutcomeCountsAfterCrashOrLostLease(t *testing.T) {
	t.Parallel()
	e := newRepairEnv(t)
	e.seed("lbr10", true)
	// A leader reserved and died before dispatching.
	_, err := e.svc.store.UpdateLBHealth(context.Background(), "lbr10", func(rec *LBDataPathRecord) (bool, error) {
		rec.Repairs = append(rec.Repairs, LBRepair{At: e.clock.Add(-time.Minute), InstanceID: "i-lbr10", Outcome: RepairReserved})
		return true, nil
	})
	require.NoError(t, err)

	e.pass()
	assert.Empty(t, e.reboots(), "the new leader sees the counted reservation")

	e.leader = false
	e.pass()
	e.leader = true
	assert.Empty(t, e.reboots())
}

func TestLBRepair_PausedBetweenEligibilityAndReservationRepairsNothing(t *testing.T) {
	t.Parallel()
	cases := map[string]func(e *repairEnv, lb *LoadBalancerRecord){
		"DeleteLoadBalancer starts": func(e *repairEnv, lb *LoadBalancerRecord) {
			require.NoError(t, e.svc.store.SuspendLBHealth(context.Background(), lb.LoadBalancerID))
		},
		"SetSubnets relaunches the VM": func(e *repairEnv, lb *LoadBalancerRecord) {
			lb.InstanceID = "i-new"
			require.NoError(t, e.svc.store.PutLoadBalancer(context.Background(), lb))
		},
		"SetSubnets rebinds the evidence": func(e *repairEnv, lb *LoadBalancerRecord) {
			require.NoError(t, e.svc.store.ResumeLBHealth(context.Background(), lb.LoadBalancerID, "i-new"))
		},
	}
	for name, interleave := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newRepairEnv(t)
			lb := e.seed("lbr11", true)
			cands := e.candidates()
			require.Len(t, cands, 1)
			require.True(t, e.rec.eligible(context.Background(), cands[0], e.clock))

			interleave(e, lb)

			assert.False(t, e.rec.repair(context.Background(), cands[0]))
			assert.Empty(t, e.reboots())
		})
	}
}

func (e *repairEnv) candidates() []lbRepairCandidate {
	e.t.Helper()
	var out []lbRepairCandidate
	lbs, err := e.svc.store.ListLoadBalancersStrict(context.Background())
	require.NoError(e.t, err)
	for _, lb := range lbs {
		rec, err := e.svc.store.GetLBHealth(context.Background(), lb.LoadBalancerID)
		require.NoError(e.t, err)
		if rec == nil {
			continue
		}
		c := lbRepairCandidate{lb: lb, rec: rec}
		for mac, nic := range rec.NICs {
			c.mac, c.nic, c.since = mac, nic, nic.FailingSince
		}
		out = append(out, c)
	}
	return out
}

// Verifies: ADR-0003:S7.
func TestS7_LBRepairActsOnlyOnTheLBsOwnVM(t *testing.T) {
	t.Parallel()
	e := newRepairEnv(t)
	e.seed("lbr12", true)
	e.seed("lbr12ok", false)
	e.pass()
	e.pass()
	assert.Equal(t, []string{"i-lbr12"}, e.reboots(), "only the failing LB's own instance ID reaches the launcher")
	e.launcher.mu.Lock()
	defer e.launcher.mu.Unlock()
	assert.Empty(t, e.launcher.terminateCalls)
	assert.Empty(t, e.launcher.launchCalls)
}

func TestLBRepair_TeardownNeverTriggersRepair(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("DeleteLoadBalancer", func(t *testing.T) {
		t.Parallel()
		e := newRepairEnv(t)
		lb := e.seed("lbr13a", true)
		require.NoError(t, e.svc.store.SuspendLBHealth(ctx, lb.LoadBalancerID))
		e.pass()
		assert.Empty(t, e.reboots())
	})
	t.Run("SetSubnets window", func(t *testing.T) {
		t.Parallel()
		e := newRepairEnv(t)
		lb := e.seed("lbr13b", true)
		require.NoError(t, e.svc.store.SuspendLBHealth(ctx, lb.LoadBalancerID))
		lb.InstanceID = "i-relaunched"
		require.NoError(t, e.svc.store.PutLoadBalancer(ctx, lb))
		e.pass()
		assert.Empty(t, e.reboots(), "suspended, and the old VM's evidence names another instance")
	})
	t.Run("transient probe failure on the relaunched VM", func(t *testing.T) {
		t.Parallel()
		e := newRepairEnv(t)
		lb := e.seed("lbr13c", false)
		lb.InstanceID = "i-relaunched"
		require.NoError(t, e.svc.store.PutLoadBalancer(ctx, lb))
		require.NoError(t, e.svc.store.ResumeLBHealth(ctx, lb.LoadBalancerID, "i-relaunched"))
		_, err := e.svc.store.UpdateLBHealth(ctx, lb.LoadBalancerID, func(rec *LBDataPathRecord) (bool, error) {
			rec.BoundAt = e.clock.Add(-90 * time.Second)
			rec.NICs = map[string]*LBNICFailure{repairMAC: {FailingSince: e.clock.Add(-80 * time.Second)}}
			return true, nil
		})
		require.NoError(t, err)
		e.pass()
		assert.Empty(t, e.reboots(), "a freshly relaunched VM gets five minutes")
	})
}
