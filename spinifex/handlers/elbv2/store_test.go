package handlers_elbv2

import (
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAccountID = "123456789012"

func setupTestStore(t *testing.T) *Store {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)

	store, err := NewStore(t.Context(), nc)
	require.NoError(t, err)
	return store
}

func newTestLB(id, name string) *LoadBalancerRecord {
	return &LoadBalancerRecord{
		LoadBalancerArn: "arn:aws:elasticloadbalancing:us-east-1:" + testAccountID + ":loadbalancer/app/" + name + "/" + id,
		LoadBalancerID:  id,
		DNSName:         name + "-" + id + ".us-east-1.elb.spinifex.local",
		Name:            name,
		Scheme:          SchemeInternal,
		Type:            LoadBalancerTypeApplication,
		State:           StateActive,
		VpcId:           "vpc-test123",
		SecurityGroups:  []string{"sg-111"},
		Subnets:         []string{"subnet-aaa"},
		IPAddressType:   IPAddressTypeIPv4,
		AccountID:       testAccountID,
		CreatedAt:       time.Now().UTC(),
	}
}

func newTestTG(id, name string) *TargetGroupRecord {
	return &TargetGroupRecord{
		TargetGroupArn: "arn:aws:elasticloadbalancing:us-east-1:" + testAccountID + ":targetgroup/" + name + "/" + id,
		TargetGroupID:  id,
		Name:           name,
		Protocol:       ProtocolHTTP,
		Port:           80,
		VpcId:          "vpc-test123",
		TargetType:     "instance",
		HealthCheck:    DefaultHealthCheck(),
		AccountID:      testAccountID,
		CreatedAt:      time.Now().UTC(),
	}
}

func newTestListener(id, lbArn string) *ListenerRecord {
	return &ListenerRecord{
		ListenerArn:     "arn:aws:elasticloadbalancing:us-east-1:" + testAccountID + ":listener/app/my-alb/lb123/" + id,
		ListenerID:      id,
		LoadBalancerArn: lbArn,
		Protocol:        ProtocolHTTP,
		Port:            80,
		DefaultActions: []ListenerAction{
			{Type: ActionTypeForward, TargetGroupArn: "arn:aws:elasticloadbalancing:us-east-1:" + testAccountID + ":targetgroup/my-tg/tg123"},
		},
		AccountID: testAccountID,
		CreatedAt: time.Now().UTC(),
	}
}

func TestLoadBalancerStoreLifecycle(t *testing.T) {
	t.Run("put and get", func(t *testing.T) {
		store := setupTestStore(t)
		require.NoError(t, store.PutLoadBalancer(t.Context(), newTestLB("getput1", "lb-getput1")))

		got, err := store.GetLoadBalancer(t.Context(), "getput1")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "getput1", got.LoadBalancerID)
	})

	t.Run("get not found", func(t *testing.T) {
		store := setupTestStore(t)
		got, err := store.GetLoadBalancer(t.Context(), "nonexistent")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("delete removes record", func(t *testing.T) {
		store := setupTestStore(t)
		require.NoError(t, store.PutLoadBalancer(t.Context(), newTestLB("del1", "lb-del1")))
		require.NoError(t, store.DeleteLoadBalancer(t.Context(), "del1"))

		got, err := store.GetLoadBalancer(t.Context(), "del1")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("delete idempotent", func(t *testing.T) {
		store := setupTestStore(t)
		require.NoError(t, store.DeleteLoadBalancer(t.Context(), "doesnt-exist"))
	})

	t.Run("list returns all", func(t *testing.T) {
		store := setupTestStore(t)
		require.NoError(t, store.PutLoadBalancer(t.Context(), newTestLB("a", "lb-a")))
		require.NoError(t, store.PutLoadBalancer(t.Context(), newTestLB("b", "lb-b")))

		records, err := store.ListLoadBalancers(t.Context())
		require.NoError(t, err)
		assert.Len(t, records, 2)
	})

	t.Run("list empty", func(t *testing.T) {
		store := setupTestStore(t)
		records, err := store.ListLoadBalancers(t.Context())
		require.NoError(t, err)
		assert.Empty(t, records)
	})
}

func TestTargetGroupStoreLifecycle(t *testing.T) {
	t.Run("put and get", func(t *testing.T) {
		store := setupTestStore(t)
		require.NoError(t, store.PutTargetGroup(t.Context(), newTestTG("getput1", "tg-getput1")))

		got, err := store.GetTargetGroup(t.Context(), "getput1")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "getput1", got.TargetGroupID)
	})

	t.Run("get not found", func(t *testing.T) {
		store := setupTestStore(t)
		got, err := store.GetTargetGroup(t.Context(), "nonexistent")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("delete removes record", func(t *testing.T) {
		store := setupTestStore(t)
		require.NoError(t, store.PutTargetGroup(t.Context(), newTestTG("del1", "tg-del1")))
		require.NoError(t, store.DeleteTargetGroup(t.Context(), "del1"))

		got, err := store.GetTargetGroup(t.Context(), "del1")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("delete idempotent", func(t *testing.T) {
		store := setupTestStore(t)
		require.NoError(t, store.DeleteTargetGroup(t.Context(), "doesnt-exist"))
	})

	t.Run("list returns all", func(t *testing.T) {
		store := setupTestStore(t)
		require.NoError(t, store.PutTargetGroup(t.Context(), newTestTG("a", "tg-a")))
		require.NoError(t, store.PutTargetGroup(t.Context(), newTestTG("b", "tg-b")))

		records, err := store.ListTargetGroups(t.Context())
		require.NoError(t, err)
		assert.Len(t, records, 2)
	})

	t.Run("list empty", func(t *testing.T) {
		store := setupTestStore(t)
		records, err := store.ListTargetGroups(t.Context())
		require.NoError(t, err)
		assert.Empty(t, records)
	})
}

func TestListenerStoreLifecycle(t *testing.T) {
	lbArn := "arn:aws:elasticloadbalancing:us-east-1:" + testAccountID + ":loadbalancer/app/test/lb1"

	t.Run("put and get", func(t *testing.T) {
		store := setupTestStore(t)
		require.NoError(t, store.PutListener(t.Context(), newTestListener("getput1", lbArn)))

		got, err := store.GetListener(t.Context(), "getput1")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "getput1", got.ListenerID)
	})

	t.Run("get not found", func(t *testing.T) {
		store := setupTestStore(t)
		got, err := store.GetListener(t.Context(), "nonexistent")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("delete removes record", func(t *testing.T) {
		store := setupTestStore(t)
		require.NoError(t, store.PutListener(t.Context(), newTestListener("del1", lbArn)))
		require.NoError(t, store.DeleteListener(t.Context(), "del1"))

		got, err := store.GetListener(t.Context(), "del1")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("delete idempotent", func(t *testing.T) {
		store := setupTestStore(t)
		require.NoError(t, store.DeleteListener(t.Context(), "doesnt-exist"))
	})

	t.Run("list returns all", func(t *testing.T) {
		store := setupTestStore(t)
		require.NoError(t, store.PutListener(t.Context(), newTestListener("a", lbArn)))
		require.NoError(t, store.PutListener(t.Context(), newTestListener("b", lbArn)))

		records, err := store.ListListeners(t.Context())
		require.NoError(t, err)
		assert.Len(t, records, 2)
	})

	t.Run("list empty", func(t *testing.T) {
		store := setupTestStore(t)
		records, err := store.ListListeners(t.Context())
		require.NoError(t, err)
		assert.Empty(t, records)
	})
}

// --- LB-specific lookups (no equivalent on TG/Listener) ---

func TestGetLoadBalancerByArn(t *testing.T) {
	store := setupTestStore(t)
	lb := newTestLB("arn123", "arn-test")
	require.NoError(t, store.PutLoadBalancer(t.Context(), lb))

	got, err := store.GetLoadBalancerByArn(t.Context(), lb.LoadBalancerArn)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, lb.Name, got.Name)

	got, err = store.GetLoadBalancerByArn(t.Context(), "arn:nonexistent")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestGetLoadBalancerByName(t *testing.T) {
	store := setupTestStore(t)
	lb := newTestLB("name123", "find-by-name")
	require.NoError(t, store.PutLoadBalancer(t.Context(), lb))

	got, err := store.GetLoadBalancerByName(t.Context(), "find-by-name", testAccountID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, lb.LoadBalancerID, got.LoadBalancerID)
}

func TestPutLoadBalancer_Update(t *testing.T) {
	store := setupTestStore(t)
	lb := newTestLB("upd123", "updatable")
	require.NoError(t, store.PutLoadBalancer(t.Context(), lb))

	lb.State = StateFailed
	lb.ENIs = []string{"eni-111", "eni-222"}
	require.NoError(t, store.PutLoadBalancer(t.Context(), lb))

	got, err := store.GetLoadBalancer(t.Context(), "upd123")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, StateFailed, got.State)
	assert.Equal(t, []string{"eni-111", "eni-222"}, got.ENIs)
}

// --- TG-specific lookups + targets ---

func TestGetTargetGroupByArn(t *testing.T) {
	store := setupTestStore(t)
	tg := newTestTG("tgarn", "arn-tg")
	require.NoError(t, store.PutTargetGroup(t.Context(), tg))

	got, err := store.GetTargetGroupByArn(t.Context(), tg.TargetGroupArn)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, tg.Name, got.Name)
}

func TestGetTargetGroupByName(t *testing.T) {
	store := setupTestStore(t)
	tg := newTestTG("tgname", "named-tg")
	require.NoError(t, store.PutTargetGroup(t.Context(), tg))

	got, err := store.GetTargetGroupByName(t.Context(), "named-tg", "vpc-test123")
	require.NoError(t, err)
	require.NotNil(t, got)

	// Wrong VPC should not find it
	got, err = store.GetTargetGroupByName(t.Context(), "named-tg", "vpc-other")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestTargetGroupWithTargets(t *testing.T) {
	store := setupTestStore(t)
	tg := newTestTG("tgtargets", "targets-tg")
	tg.Targets = []Target{
		{Id: "i-aaa111", Port: 8080, HealthState: TargetHealthInitial, PrivateIP: "10.0.1.10"},
		{Id: "i-bbb222", Port: 0, HealthState: TargetHealthHealthy, PrivateIP: "10.0.1.11"},
	}
	require.NoError(t, store.PutTargetGroup(t.Context(), tg))

	got, err := store.GetTargetGroup(t.Context(), "tgtargets")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Len(t, got.Targets, 2)
	assert.Equal(t, "i-aaa111", got.Targets[0].Id)
	assert.Equal(t, int64(8080), got.Targets[0].Port)
	assert.Equal(t, "10.0.1.11", got.Targets[1].PrivateIP)
}

// --- Listener-specific lookups ---

func TestListListenersByLB(t *testing.T) {
	store := setupTestStore(t)
	lbArn1 := "arn:aws:elasticloadbalancing:us-east-1:" + testAccountID + ":loadbalancer/app/alb1/lb1"
	lbArn2 := "arn:aws:elasticloadbalancing:us-east-1:" + testAccountID + ":loadbalancer/app/alb2/lb2"

	l1 := newTestListener("lst1", lbArn1)
	l1.Port = 80
	l2 := newTestListener("lst2", lbArn1)
	l2.Port = 443
	l3 := newTestListener("lst3", lbArn2)
	l3.Port = 80

	require.NoError(t, store.PutListener(t.Context(), l1))
	require.NoError(t, store.PutListener(t.Context(), l2))
	require.NoError(t, store.PutListener(t.Context(), l3))

	listeners, err := store.ListListenersByLB(t.Context(), lbArn1)
	require.NoError(t, err)
	assert.Len(t, listeners, 2)

	listeners, err = store.ListListenersByLB(t.Context(), lbArn2)
	require.NoError(t, err)
	assert.Len(t, listeners, 1)
}

func TestGetListenerByArn(t *testing.T) {
	store := setupTestStore(t)
	lbArn := "arn:aws:elasticloadbalancing:us-east-1:" + testAccountID + ":loadbalancer/app/test/lb1"
	l := newTestListener("lstarn", lbArn)
	require.NoError(t, store.PutListener(t.Context(), l))

	got, err := store.GetListenerByArn(t.Context(), l.ListenerArn)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, l.ListenerID, got.ListenerID)

	got, err = store.GetListenerByArn(t.Context(), "arn:nonexistent")
	require.NoError(t, err)
	assert.Nil(t, got)

	// ARN that parses to a real listener ID but a different ARN must not match
	// (defence-in-depth against the parse trick serving a wrong record).
	got, err = store.GetListenerByArn(t.Context(), "arn:aws:elasticloadbalancing:us-east-1:"+testAccountID+":listener/app/other/lbX/"+l.ListenerID)
	require.NoError(t, err)
	assert.Nil(t, got)
}

// --- Cross-resource isolation: shared IDs across record types must not collide ---

func TestResourceIsolation(t *testing.T) {
	store := setupTestStore(t)

	lb := newTestLB("shared1", "alb-shared")
	tg := newTestTG("shared1", "tg-shared") // Same ID as LB
	l := newTestListener("shared1", lb.LoadBalancerArn)

	require.NoError(t, store.PutLoadBalancer(t.Context(), lb))
	require.NoError(t, store.PutTargetGroup(t.Context(), tg))
	require.NoError(t, store.PutListener(t.Context(), l))

	gotLB, err := store.GetLoadBalancer(t.Context(), "shared1")
	require.NoError(t, err)
	require.NotNil(t, gotLB)
	assert.Equal(t, "alb-shared", gotLB.Name)

	gotTG, err := store.GetTargetGroup(t.Context(), "shared1")
	require.NoError(t, err)
	require.NotNil(t, gotTG)
	assert.Equal(t, "tg-shared", gotTG.Name)

	gotL, err := store.GetListener(t.Context(), "shared1")
	require.NoError(t, err)
	require.NotNil(t, gotL)
	assert.Equal(t, ProtocolHTTP, gotL.Protocol)

	// Deleting one type should not affect others
	require.NoError(t, store.DeleteLoadBalancer(t.Context(), "shared1"))
	gotTG, _ = store.GetTargetGroup(t.Context(), "shared1")
	assert.NotNil(t, gotTG)
	gotL, _ = store.GetListener(t.Context(), "shared1")
	assert.NotNil(t, gotL)
}

func TestTargetGroupsForLB(t *testing.T) {
	store := setupTestStore(t)

	// Non-existent LB returns nil, nil
	tgs, err := store.TargetGroupsForLB(t.Context(), "nonexistent")
	require.NoError(t, err)
	assert.Nil(t, tgs)

	lb := newTestLB("tgflb1", "my-alb")
	tg := newTestTG("tg001", "my-tg")
	require.NoError(t, store.PutLoadBalancer(t.Context(), lb))
	require.NoError(t, store.PutTargetGroup(t.Context(), tg))

	listener := &ListenerRecord{
		ListenerArn:     "arn:aws:elasticloadbalancing:us-east-1:" + testAccountID + ":listener/app/my-alb/tgflb1/lst1",
		ListenerID:      "lst1",
		LoadBalancerArn: lb.LoadBalancerArn,
		Protocol:        ProtocolHTTP,
		Port:            80,
		DefaultActions: []ListenerAction{
			{Type: ActionTypeForward, TargetGroupArn: tg.TargetGroupArn},
			{Type: ActionTypeForward, TargetGroupArn: ""}, // empty ARN should be skipped
		},
		AccountID: testAccountID,
	}
	require.NoError(t, store.PutListener(t.Context(), listener))

	tgs, err = store.TargetGroupsForLB(t.Context(), "tgflb1")
	require.NoError(t, err)
	require.Len(t, tgs, 1)
	assert.Equal(t, "tg001", tgs[0].TargetGroupID)
}
