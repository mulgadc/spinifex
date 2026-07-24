package handlers_elbv2

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/lbagent"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- evaluateHealth ---

func TestEvaluateHealth_InitialToHealthy(t *testing.T) {
	cfg := DefaultHealthCheck()
	ctr := &targetCounter{consecutiveHealthy: 1}

	state, desc := evaluateHealth(TargetHealthInitial, ctr, cfg)
	assert.Equal(t, TargetHealthHealthy, state)
	assert.Equal(t, "Target is healthy", desc)
}

func TestEvaluateHealth_InitialToUnhealthy(t *testing.T) {
	cfg := DefaultHealthCheck()
	ctr := &targetCounter{consecutiveUnhealthy: cfg.UnhealthyThreshold}

	state, _ := evaluateHealth(TargetHealthInitial, ctr, cfg)
	assert.Equal(t, TargetHealthUnhealthy, state)
}

func TestEvaluateHealth_InitialStaysInitial(t *testing.T) {
	cfg := DefaultHealthCheck()
	ctr := &targetCounter{consecutiveUnhealthy: 1} // below threshold

	state, _ := evaluateHealth(TargetHealthInitial, ctr, cfg)
	assert.Equal(t, TargetHealthInitial, state)
}

func TestEvaluateHealth_HealthyToUnhealthy(t *testing.T) {
	cfg := DefaultHealthCheck()
	ctr := &targetCounter{consecutiveUnhealthy: cfg.UnhealthyThreshold}

	state, _ := evaluateHealth(TargetHealthHealthy, ctr, cfg)
	assert.Equal(t, TargetHealthUnhealthy, state)
}

func TestEvaluateHealth_HealthyStaysHealthy(t *testing.T) {
	cfg := DefaultHealthCheck()
	ctr := &targetCounter{consecutiveUnhealthy: 1} // below threshold

	state, _ := evaluateHealth(TargetHealthHealthy, ctr, cfg)
	assert.Equal(t, TargetHealthHealthy, state)
}

func TestEvaluateHealth_UnhealthyToHealthy(t *testing.T) {
	cfg := DefaultHealthCheck()
	ctr := &targetCounter{consecutiveHealthy: cfg.HealthyThreshold}

	state, _ := evaluateHealth(TargetHealthUnhealthy, ctr, cfg)
	assert.Equal(t, TargetHealthHealthy, state)
}

func TestEvaluateHealth_UnhealthyStaysUnhealthy(t *testing.T) {
	cfg := DefaultHealthCheck()
	ctr := &targetCounter{consecutiveHealthy: cfg.HealthyThreshold - 1}

	state, _ := evaluateHealth(TargetHealthUnhealthy, ctr, cfg)
	assert.Equal(t, TargetHealthUnhealthy, state)
}

func TestEvaluateHealth_DrainingUnchanged(t *testing.T) {
	cfg := DefaultHealthCheck()
	ctr := &targetCounter{consecutiveHealthy: 100}

	state, _ := evaluateHealth(TargetHealthDraining, ctr, cfg)
	assert.Equal(t, TargetHealthDraining, state)
}

// --- integration: handleHealthReport directly ---

func setupTestNATS(t *testing.T) *Store {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	store, err := NewStore(t.Context(), nc)
	require.NoError(t, err)
	return store
}

// setupLBWithTG creates a load balancer, listener, and target group wired
// together so that TargetGroupsForLB can resolve TGs from the LBID.
func setupLBWithTG(t *testing.T, store *Store, lbID string, tg *TargetGroupRecord) {
	t.Helper()
	lbArn := "arn:aws:elasticloadbalancing:us-east-1:000:loadbalancer/app/test/" + lbID
	require.NoError(t, store.PutLoadBalancer(t.Context(), &LoadBalancerRecord{
		LoadBalancerArn: lbArn,
		LoadBalancerID:  lbID,
		Name:            "test-lb",
		State:           StateActive,
	}))
	require.NoError(t, store.PutListener(t.Context(), &ListenerRecord{
		ListenerArn:     lbArn + "/listener-1",
		ListenerID:      lbID + "-lis",
		LoadBalancerArn: lbArn,
		Protocol:        "HTTP",
		Port:            80,
		DefaultActions: []ListenerAction{
			{Type: ActionTypeForward, TargetGroupArn: tg.TargetGroupArn},
		},
	}))
}

func TestHandleHealthReport_TransitionsInitialToHealthy(t *testing.T) {
	store := setupTestNATS(t)

	hc := newHealthChecker(store)

	tg := &TargetGroupRecord{
		TargetGroupArn: "arn:aws:elasticloadbalancing:us-east-1:000:targetgroup/test/tg-123",
		TargetGroupID:  "tg-123",
		Port:           80,
		HealthCheck:    DefaultHealthCheck(),
		Targets: []Target{
			{Id: "i-aaa111", Port: 80, HealthState: TargetHealthInitial, PrivateIP: "10.0.1.10"},
		},
	}
	require.NoError(t, store.PutTargetGroup(t.Context(), tg))
	setupLBWithTG(t, store, "lb-test1", tg)

	report := lbagent.HealthReport{
		LBID: "lb-test1",
		Servers: []lbagent.ServerStatus{
			{Backend: "bk_tg-123", Server: sanitizeName("srv", "i-aaa111"), Status: "UP"},
		},
	}
	hc.handleHealthReportDirect(context.Background(), report)

	stored, err := store.GetTargetGroup(t.Context(), "tg-123")
	require.NoError(t, err)
	assert.Equal(t, TargetHealthHealthy, stored.Targets[0].HealthState)
}

func TestHandleHealthReport_UnhealthyAfterThreshold(t *testing.T) {
	store := setupTestNATS(t)

	hc := newHealthChecker(store)

	tg := &TargetGroupRecord{
		TargetGroupArn: "arn:aws:elasticloadbalancing:us-east-1:000:targetgroup/test/tg-456",
		TargetGroupID:  "tg-456",
		Port:           80,
		HealthCheck: HealthCheckConfig{
			UnhealthyThreshold: 2,
			HealthyThreshold:   5,
		},
		Targets: []Target{
			{Id: "i-bbb222", Port: 80, HealthState: TargetHealthInitial, PrivateIP: "10.0.1.11"},
		},
	}
	require.NoError(t, store.PutTargetGroup(t.Context(), tg))
	setupLBWithTG(t, store, "lb-test2", tg)

	srvName := sanitizeName("srv", "i-bbb222")

	// Send 2 DOWN reports to hit the unhealthy threshold of 2
	for range 2 {
		report := lbagent.HealthReport{
			LBID: "lb-test2",
			Servers: []lbagent.ServerStatus{
				{Backend: "bk_tg-456", Server: srvName, Status: "DOWN"},
			},
		}
		hc.handleHealthReportDirect(context.Background(), report)
	}

	stored, err := store.GetTargetGroup(t.Context(), "tg-456")
	require.NoError(t, err)
	assert.Equal(t, TargetHealthUnhealthy, stored.Targets[0].HealthState)
}

func TestHandleHealthReport_SkipsDrainingTargets(t *testing.T) {
	store := setupTestNATS(t)

	hc := newHealthChecker(store)

	tg := &TargetGroupRecord{
		TargetGroupArn: "arn:aws:elasticloadbalancing:us-east-1:000:targetgroup/test/tg-789",
		TargetGroupID:  "tg-789",
		Port:           80,
		HealthCheck:    DefaultHealthCheck(),
		Targets: []Target{
			{Id: "i-drain", Port: 80, HealthState: TargetHealthDraining, PrivateIP: "10.0.0.1"},
		},
	}
	require.NoError(t, store.PutTargetGroup(t.Context(), tg))
	setupLBWithTG(t, store, "lb-test3", tg)

	report := lbagent.HealthReport{
		LBID: "lb-test3",
		Servers: []lbagent.ServerStatus{
			{Backend: "bk_tg-789", Server: sanitizeName("srv", "i-drain"), Status: "UP"},
		},
	}
	hc.handleHealthReportDirect(context.Background(), report)

	stored, err := store.GetTargetGroup(t.Context(), "tg-789")
	require.NoError(t, err)
	assert.Equal(t, TargetHealthDraining, stored.Targets[0].HealthState)
}

func TestRemoveTarget(t *testing.T) {
	hc := newHealthChecker(nil)

	hc.mu.Lock()
	hc.counters["tg-1:i-aaa:80"] = &targetCounter{consecutiveHealthy: 5}
	hc.mu.Unlock()

	hc.removeTarget("tg-1", "i-aaa", 80)

	hc.mu.Lock()
	_, exists := hc.counters["tg-1:i-aaa:80"]
	hc.mu.Unlock()
	assert.False(t, exists)
}

func TestHandleHealthReport_EmptyServers(t *testing.T) {
	store := setupTestNATS(t)
	hc := newHealthChecker(store)

	report := lbagent.HealthReport{LBID: "lb-empty", Servers: nil}
	// Should return early without touching the store.
	hc.handleHealthReportDirect(context.Background(), report)
}

func TestHandleHealthReport_TargetPortZeroUsesTGPort(t *testing.T) {
	store := setupTestNATS(t)
	hc := newHealthChecker(store)

	tg := &TargetGroupRecord{
		TargetGroupArn: "arn:aws:elasticloadbalancing:us-east-1:000:targetgroup/test/tg-p0",
		TargetGroupID:  "tg-p0",
		Port:           8080,
		HealthCheck:    DefaultHealthCheck(),
		Targets: []Target{
			{Id: "i-port0", Port: 0, HealthState: TargetHealthInitial, PrivateIP: "10.0.0.50"},
		},
	}
	require.NoError(t, store.PutTargetGroup(t.Context(), tg))
	setupLBWithTG(t, store, "lb-p0", tg)

	report := lbagent.HealthReport{
		LBID: "lb-p0",
		Servers: []lbagent.ServerStatus{
			{Backend: "bk_tg-p0", Server: sanitizeName("srv", "i-port0"), Status: "UP"},
		},
	}
	hc.handleHealthReportDirect(context.Background(), report)

	stored, err := store.GetTargetGroup(t.Context(), "tg-p0")
	require.NoError(t, err)
	assert.Equal(t, TargetHealthHealthy, stored.Targets[0].HealthState)

	// Verify the counter key uses the TG port (8080) not target port (0)
	hc.mu.Lock()
	_, ok := hc.counters["tg-p0:i-port0:8080"]
	hc.mu.Unlock()
	assert.True(t, ok, "counter key should use TG default port 8080")
}

func TestHandleHealthReportDirect_TransitionsInitialToHealthy(t *testing.T) {
	store := setupTestNATS(t)
	hc := newHealthChecker(store)

	tg := &TargetGroupRecord{
		TargetGroupArn: "arn:aws:elasticloadbalancing:us-east-1:000:targetgroup/test/tg-direct",
		TargetGroupID:  "tg-direct",
		Port:           80,
		HealthCheck:    DefaultHealthCheck(),
		Targets: []Target{
			{Id: "i-direct1", Port: 80, HealthState: TargetHealthInitial, PrivateIP: "10.0.1.20"},
		},
	}
	require.NoError(t, store.PutTargetGroup(t.Context(), tg))
	setupLBWithTG(t, store, "lb-direct", tg)

	// Call handleHealthReportDirect with a struct — no JSON round-trip.
	hc.handleHealthReportDirect(context.Background(), lbagent.HealthReport{
		LBID: "lb-direct",
		Servers: []lbagent.ServerStatus{
			{Backend: "bk_tg-direct", Server: sanitizeName("srv", "i-direct1"), Status: "UP"},
		},
	})

	stored, err := store.GetTargetGroup(t.Context(), "tg-direct")
	require.NoError(t, err)
	assert.Equal(t, TargetHealthHealthy, stored.Targets[0].HealthState)
}

func TestHandleHealthReportDirect_EmptyServersIsNoOp(t *testing.T) {
	store := setupTestNATS(t)
	hc := newHealthChecker(store)

	// Should return immediately without touching the store.
	hc.handleHealthReportDirect(context.Background(), lbagent.HealthReport{LBID: "lb-empty", Servers: nil})
}

func TestHandleHealthReport_OnlyProcessesTGsForReportingLB(t *testing.T) {
	store := setupTestNATS(t)
	hc := newHealthChecker(store)

	// TG attached to lb-A
	tgA := &TargetGroupRecord{
		TargetGroupArn: "arn:aws:elasticloadbalancing:us-east-1:000:targetgroup/test/tg-a",
		TargetGroupID:  "tg-a",
		Port:           80,
		HealthCheck:    DefaultHealthCheck(),
		Targets: []Target{
			{Id: "i-shared", Port: 80, HealthState: TargetHealthInitial, PrivateIP: "10.0.0.1"},
		},
	}
	require.NoError(t, store.PutTargetGroup(t.Context(), tgA))
	setupLBWithTG(t, store, "lb-A", tgA)

	// TG attached to lb-B — same target ID, different TG
	tgB := &TargetGroupRecord{
		TargetGroupArn: "arn:aws:elasticloadbalancing:us-east-1:000:targetgroup/test/tg-b",
		TargetGroupID:  "tg-b",
		Port:           80,
		HealthCheck:    DefaultHealthCheck(),
		Targets: []Target{
			{Id: "i-shared", Port: 80, HealthState: TargetHealthInitial, PrivateIP: "10.0.0.2"},
		},
	}
	require.NoError(t, store.PutTargetGroup(t.Context(), tgB))
	setupLBWithTG(t, store, "lb-B", tgB)

	// Report from lb-A — only tg-a should be updated.
	hc.handleHealthReportDirect(context.Background(), lbagent.HealthReport{
		LBID: "lb-A",
		Servers: []lbagent.ServerStatus{
			{Backend: "bk_tg-a", Server: sanitizeName("srv", "i-shared"), Status: "UP"},
		},
	})

	storedA, err := store.GetTargetGroup(t.Context(), "tg-a")
	require.NoError(t, err)
	assert.Equal(t, TargetHealthHealthy, storedA.Targets[0].HealthState, "tg-a should be updated")

	storedB, err := store.GetTargetGroup(t.Context(), "tg-b")
	require.NoError(t, err)
	assert.Equal(t, TargetHealthInitial, storedB.Targets[0].HealthState, "tg-b must NOT be updated by lb-A's report")
}

func TestHandleHealthReport_FallbackListTargetGroups(t *testing.T) {
	store := setupTestNATS(t)
	hc := newHealthChecker(store)

	// Store a TG with a target — no LB linkage needed because empty LBID
	// triggers the ListTargetGroups fallback path.
	tg := &TargetGroupRecord{
		TargetGroupArn: "arn:aws:elasticloadbalancing:us-east-1:000:targetgroup/test/tg-fb",
		TargetGroupID:  "tg-fb",
		Port:           80,
		HealthCheck:    DefaultHealthCheck(),
		Targets: []Target{
			{Id: "i-fallback", Port: 80, HealthState: TargetHealthInitial, PrivateIP: "10.0.0.99"},
		},
	}
	require.NoError(t, store.PutTargetGroup(t.Context(), tg))

	// Report with empty LBID → should fall back to ListTargetGroups
	hc.handleHealthReportDirect(context.Background(), lbagent.HealthReport{
		LBID: "",
		Servers: []lbagent.ServerStatus{
			{Backend: "bk_tg-fb", Server: sanitizeName("srv", "i-fallback"), Status: "UP"},
			{Backend: "bk_tg-fb", Server: "srv_unknown", Status: "UP"}, // unknown server → skipped
		},
	})

	stored, err := store.GetTargetGroup(t.Context(), "tg-fb")
	require.NoError(t, err)
	assert.Equal(t, TargetHealthHealthy, stored.Targets[0].HealthState)
}

func TestEvaluateHealth_ZeroThresholdsUsesDefaults(t *testing.T) {
	cfg := HealthCheckConfig{} // all zeros

	// Unhealthy→healthy should require DefaultHealthyThreshold consecutive healthy
	ctr := &targetCounter{consecutiveHealthy: DefaultHealthyThreshold}
	state, _ := evaluateHealth(TargetHealthUnhealthy, ctr, cfg)
	assert.Equal(t, TargetHealthHealthy, state)

	// Healthy→unhealthy should require DefaultUnhealthyThreshold consecutive unhealthy
	ctr2 := &targetCounter{consecutiveUnhealthy: DefaultUnhealthyThreshold}
	state2, _ := evaluateHealth(TargetHealthHealthy, ctr2, cfg)
	assert.Equal(t, TargetHealthUnhealthy, state2)
}
