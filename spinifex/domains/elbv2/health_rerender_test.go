package elbv2

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
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
		AccountID:       testAccountID,
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

func TestUpdateLoadBalancer_RetriesAfterConcurrentWriteAndKeepsBoth(t *testing.T) {
	t.Parallel()
	svc := setupRerender(t, "lb-a")
	attempts := 0
	heartbeat := time.Now().UTC().Truncate(time.Second)

	_, err := svc.store.UpdateLoadBalancer(t.Context(), "lb-a", func(rec *LoadBalancerRecord) (bool, error) {
		attempts++
		if attempts == 1 {
			_, werr := svc.store.UpdateLoadBalancer(t.Context(), "lb-a", func(r *LoadBalancerRecord) (bool, error) {
				r.ConfigHash = "rendered-elsewhere"
				return true, nil
			})
			require.NoError(t, werr)
		}
		rec.LastHeartbeat = heartbeat
		return true, nil
	})
	require.NoError(t, err)

	assert.Equal(t, 2, attempts)
	got := getLB(t, svc, "lb-a")
	assert.Equal(t, "rendered-elsewhere", got.ConfigHash)
	assert.True(t, heartbeat.Equal(got.LastHeartbeat))
}

func TestUpdateLoadBalancer_MissingRecordIsNotCreated(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	_, err := svc.store.UpdateLoadBalancer(t.Context(), "lb-gone", func(rec *LoadBalancerRecord) (bool, error) {
		return true, nil
	})
	require.ErrorIs(t, err, ErrLoadBalancerNotFound)
	got, gerr := svc.store.GetLoadBalancer(t.Context(), "lb-gone")
	require.NoError(t, gerr)
	assert.Nil(t, got)
}

func TestReRender_KeepsHeartbeatWrittenAfterItReadTheLB(t *testing.T) {
	t.Parallel()
	svc := setupRerender(t, "lb-a")
	stale := getLB(t, svc, "lb-a")

	heartbeat := time.Now().UTC().Truncate(time.Second)
	_, err := svc.store.UpdateLoadBalancer(t.Context(), "lb-a", func(rec *LoadBalancerRecord) (bool, error) {
		rec.LastHeartbeat = heartbeat
		return true, nil
	})
	require.NoError(t, err)

	_, err = svc.store.UpdateTargetGroup(t.Context(), "tg-rr", func(tg *TargetGroupRecord) (bool, error) {
		tg.Targets[1].HealthState = TargetHealthUnhealthy
		return true, nil
	})
	require.NoError(t, err)
	require.NoError(t, svc.updateStoredConfig(t.Context(), stale))

	got := getLB(t, svc, "lb-a")
	assert.True(t, heartbeat.Equal(got.LastHeartbeat), "re-render must not roll back the heartbeat")
	assert.NotContains(t, got.ConfigText, "10.0.1.11")
}

func TestHeartbeatRefresh_KeepsReRenderedConfig(t *testing.T) {
	t.Parallel()
	svc := setupRerender(t, "lb-a")
	svc.hc.handleHealthReportDirect(t.Context(), rerenderReport("lb-a", "DOWN"))
	rendered := getLB(t, svc, "lb-a")
	require.NotContains(t, rendered.ConfigText, "10.0.1.11")

	_, err := svc.LBAgentHeartbeat(t.Context(), &LBAgentHeartbeatInput{LBID: aws.String("lb-a")}, testAccountID)
	require.NoError(t, err)

	got := getLB(t, svc, "lb-a")
	assert.Equal(t, rendered.ConfigHash, got.ConfigHash)
	assert.Equal(t, rendered.ConfigText, got.ConfigText)
	assert.True(t, got.LastHeartbeat.After(rendered.LastHeartbeat), "stale heartbeat must be refreshed")
}

const rerenderLBArn = "arn:aws:elasticloadbalancing:us-east-1:1:loadbalancer/net/lb-a/lb-a"

// bumpConfigHash simulates a re-render landing after another writer read the LB.
func bumpConfigHash(t *testing.T, svc *ELBv2ServiceImpl, lbID string) {
	t.Helper()
	_, err := svc.store.UpdateLoadBalancer(t.Context(), lbID, func(rec *LoadBalancerRecord) (bool, error) {
		rec.ConfigHash = "rendered-elsewhere"
		return true, nil
	})
	require.NoError(t, err)
}

func TestLBAttributeSave_KeepsConfigHashWrittenAfterRead(t *testing.T) {
	t.Parallel()
	svc := setupRerender(t, "lb-a")
	res := svc.lbAttrResource(t.Context(), rerenderLBArn, testAccountID, "test")
	stale, err := res.fetch(rerenderLBArn)
	require.NoError(t, err)
	require.NotNil(t, stale)

	bumpConfigHash(t, svc, "lb-a")
	stale.Attributes = map[string]string{"deletion_protection.enabled": "true"}
	require.NoError(t, res.save(stale))

	got := getLB(t, svc, "lb-a")
	assert.Equal(t, "rendered-elsewhere", got.ConfigHash)
	assert.Equal(t, "true", got.Attributes["deletion_protection.enabled"])
}

func TestLBTagSave_KeepsConfigHashWrittenAfterRead(t *testing.T) {
	t.Parallel()
	svc := setupRerender(t, "lb-a")
	h, _, found, err := svc.resolveTaggable(t.Context(), rerenderLBArn)
	require.NoError(t, err)
	require.True(t, found)

	bumpConfigHash(t, svc, "lb-a")
	require.NoError(t, h.save(map[string]string{"env": "test"}))

	got := getLB(t, svc, "lb-a")
	assert.Equal(t, "rendered-elsewhere", got.ConfigHash)
	assert.Equal(t, "test", got.Tags["env"])
}

// The next three read and write inside one call, so the hash is bumped first:
// they prove only the fields they own change, not the interleaving itself.
func TestSetIpAddressType_LeavesConfigHashAlone(t *testing.T) {
	t.Parallel()
	svc := setupRerender(t, "lb-a")
	bumpConfigHash(t, svc, "lb-a")

	_, err := svc.SetIpAddressType(t.Context(), &elbv2.SetIpAddressTypeInput{
		LoadBalancerArn: aws.String(rerenderLBArn),
		IpAddressType:   aws.String(IPAddressTypeIPv4),
	}, testAccountID)
	require.NoError(t, err)

	got := getLB(t, svc, "lb-a")
	assert.Equal(t, "rendered-elsewhere", got.ConfigHash)
	assert.Equal(t, IPAddressTypeIPv4, got.IPAddressType)
}

func TestSetSecurityGroups_LeavesConfigHashAlone(t *testing.T) {
	t.Parallel()
	svc := setupRerender(t, "lb-a")
	_, err := svc.store.UpdateLoadBalancer(t.Context(), "lb-a", func(rec *LoadBalancerRecord) (bool, error) {
		rec.SecurityGroups = []string{"sg-old"}
		rec.ConfigHash = "rendered-elsewhere"
		return true, nil
	})
	require.NoError(t, err)

	_, err = svc.SetSecurityGroups(t.Context(), &elbv2.SetSecurityGroupsInput{
		LoadBalancerArn: aws.String(rerenderLBArn),
		SecurityGroups:  []*string{aws.String("sg-new")},
	}, testAccountID)
	require.NoError(t, err)

	got := getLB(t, svc, "lb-a")
	assert.Equal(t, "rendered-elsewhere", got.ConfigHash)
	assert.Equal(t, []string{"sg-new"}, got.SecurityGroups)
}

func TestSetLoadBalancerIngressCIDRs_LeavesConfigHashAlone(t *testing.T) {
	t.Parallel()
	svc := setupRerender(t, "lb-a")
	bumpConfigHash(t, svc, "lb-a")

	require.NoError(t, svc.SetLoadBalancerIngressCIDRs(rerenderLBArn, []string{"10.0.0.0/8"}, testAccountID))

	got := getLB(t, svc, "lb-a")
	assert.Equal(t, "rendered-elsewhere", got.ConfigHash)
	assert.Equal(t, []string{"10.0.0.0/8"}, got.NLBIngressCIDRs)
}
