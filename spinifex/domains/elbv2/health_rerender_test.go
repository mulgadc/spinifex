package elbv2

import (
	"testing"

	lbagent "github.com/mulgadc/spinifex/spinifex/agents/elbv2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const rerenderTGArn = "arn:aws:elasticloadbalancing:us-east-1:1:targetgroup/db/tg-rr"

// putRerenderNLB stores an NLB with a TCP listener forwarding to rerenderTGArn
// and renders its initial config.
func putRerenderNLB(t *testing.T, svc *ELBv2ServiceImpl, lbID string) {
	t.Helper()
	lbArn := "arn:aws:elasticloadbalancing:us-east-1:1:loadbalancer/net/" + lbID + "/" + lbID
	lb := &LoadBalancerRecord{
		LoadBalancerArn: lbArn,
		LoadBalancerID:  lbID,
		Name:            lbID,
		Type:            LoadBalancerTypeNetwork,
		State:           StateActive,
		InstanceID:      "i-" + lbID,
	}
	require.NoError(t, svc.store.PutLoadBalancer(t.Context(), lb))
	l := tcpListener(lbArn+"/l1", lbID+"-l1", ProtocolTCP, 5432, rerenderTGArn)
	l.LoadBalancerArn = lbArn
	require.NoError(t, svc.store.PutListener(t.Context(), l))
	require.NoError(t, svc.updateStoredConfig(t.Context(), lb))
}

func setupRerender(t *testing.T, lbIDs ...string) *ELBv2ServiceImpl {
	t.Helper()
	svc := setupTestService(t)
	hcCfg := nlbHealthCheck()
	hcCfg.HealthyThreshold = 1
	hcCfg.UnhealthyThreshold = 1
	require.NoError(t, svc.store.PutTargetGroup(t.Context(), &TargetGroupRecord{
		TargetGroupArn: rerenderTGArn,
		TargetGroupID:  "tg-rr",
		Protocol:       ProtocolTCP,
		Port:           5432,
		HealthCheck:    hcCfg,
		Targets: []Target{
			{Id: "i-ok", Port: 5432, PrivateIP: "10.0.1.10", HealthState: TargetHealthHealthy},
			{Id: "i-bad", Port: 5432, PrivateIP: "10.0.1.11", HealthState: TargetHealthHealthy},
		},
	}))
	for _, id := range lbIDs {
		putRerenderNLB(t, svc, id)
	}
	return svc
}

func rerenderReport(lbID, badStatus string) lbagent.HealthReport {
	return lbagent.HealthReport{
		LBID: lbID,
		Servers: []lbagent.ServerStatus{
			{Server: sanitizeName("srv", "i-ok"), Status: "UP"},
			{Server: sanitizeName("srv", "i-bad"), Status: badStatus},
		},
	}
}

func getLB(t *testing.T, svc *ELBv2ServiceImpl, id string) *LoadBalancerRecord {
	t.Helper()
	lb, err := svc.store.GetLoadBalancer(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, lb)
	return lb
}

func TestHealthTransition_ReRendersConfigWithoutUnhealthyTarget(t *testing.T) {
	t.Parallel()
	svc := setupRerender(t, "lb-a")
	before := getLB(t, svc, "lb-a")
	require.Contains(t, before.ConfigText, "10.0.1.11")

	svc.hc.handleHealthReportDirect(t.Context(), rerenderReport("lb-a", "DOWN"))

	after := getLB(t, svc, "lb-a")
	assert.NotEqual(t, before.ConfigHash, after.ConfigHash)
	assert.NotContains(t, after.ConfigText, "10.0.1.11")
	assert.Contains(t, after.ConfigText, "10.0.1.10")
}

func TestHealthReport_SteadyStateLeavesLBUntouched(t *testing.T) {
	t.Parallel()
	svc := setupRerender(t, "lb-a")
	before := getLB(t, svc, "lb-a")

	svc.hc.handleHealthReportDirect(t.Context(), rerenderReport("lb-a", "UP"))

	assert.Equal(t, before, getLB(t, svc, "lb-a"))
}

func TestHealthTransition_SharedTargetGroupReRendersEveryLB(t *testing.T) {
	t.Parallel()
	svc := setupRerender(t, "lb-a", "lb-b")

	svc.hc.handleHealthReportDirect(t.Context(), rerenderReport("lb-a", "DOWN"))

	for _, id := range []string{"lb-a", "lb-b"} {
		assert.NotContains(t, getLB(t, svc, id).ConfigText, "10.0.1.11", id)
	}
}

func TestHealthTransition_RecoveryReAddsTarget(t *testing.T) {
	t.Parallel()
	svc := setupRerender(t, "lb-a")
	svc.hc.handleHealthReportDirect(t.Context(), rerenderReport("lb-a", "DOWN"))
	require.NotContains(t, getLB(t, svc, "lb-a").ConfigText, "10.0.1.11")

	svc.hc.handleHealthReportDirect(t.Context(), rerenderReport("lb-a", "UP"))

	assert.Contains(t, getLB(t, svc, "lb-a").ConfigText, "10.0.1.11")
}
