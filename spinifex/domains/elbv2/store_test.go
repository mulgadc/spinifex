package elbv2

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
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

// TestStore_WatchBucketOpensAWatchableHandle closes the loop: the returned
// bucket is not merely named, it opens against the live server so the DNS
// reconcile can actually be woken by a load-balancer change.
func TestStore_WatchBucketOpensAWatchableHandle(t *testing.T) {
	t.Parallel()
	store := setupTestStore(t)

	bucket := store.WatchBucket()
	require.NotNil(t, bucket)
	assert.Equal(t, KVBucketELBv2, bucket.Name())

	watcher, err := bucket.Watch(t.Context(), KeyPrefixLB+"*")
	require.NoError(t, err)
	require.NoError(t, watcher.Stop())
}

func TestLoadBalancerStoreLifecycle(t *testing.T) {
	t.Parallel()
	t.Run("put and get", func(t *testing.T) {
		t.Parallel()
		store := setupTestStore(t)
		require.NoError(t, store.PutLoadBalancer(t.Context(), newTestLB("getput1", "lb-getput1")))

		got, err := store.GetLoadBalancer(t.Context(), "getput1")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "getput1", got.LoadBalancerID)
	})

	t.Run("get not found", func(t *testing.T) {
		t.Parallel()
		store := setupTestStore(t)
		got, err := store.GetLoadBalancer(t.Context(), "nonexistent")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("delete removes record", func(t *testing.T) {
		t.Parallel()
		store := setupTestStore(t)
		require.NoError(t, store.PutLoadBalancer(t.Context(), newTestLB("del1", "lb-del1")))
		require.NoError(t, store.DeleteLoadBalancer(t.Context(), "del1"))

		got, err := store.GetLoadBalancer(t.Context(), "del1")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("delete idempotent", func(t *testing.T) {
		t.Parallel()
		store := setupTestStore(t)
		require.NoError(t, store.DeleteLoadBalancer(t.Context(), "doesnt-exist"))
	})

	t.Run("list returns all", func(t *testing.T) {
		t.Parallel()
		store := setupTestStore(t)
		require.NoError(t, store.PutLoadBalancer(t.Context(), newTestLB("a", "lb-a")))
		require.NoError(t, store.PutLoadBalancer(t.Context(), newTestLB("b", "lb-b")))

		records, err := store.ListLoadBalancers(t.Context())
		require.NoError(t, err)
		assert.Len(t, records, 2)
	})

	t.Run("list empty", func(t *testing.T) {
		t.Parallel()
		store := setupTestStore(t)
		records, err := store.ListLoadBalancers(t.Context())
		require.NoError(t, err)
		assert.Empty(t, records)
	})
}

func TestTargetGroupStoreLifecycle(t *testing.T) {
	t.Parallel()
	t.Run("put and get", func(t *testing.T) {
		t.Parallel()
		store := setupTestStore(t)
		require.NoError(t, store.PutTargetGroup(t.Context(), newTestTG("getput1", "tg-getput1")))

		got, err := store.GetTargetGroup(t.Context(), "getput1")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "getput1", got.TargetGroupID)
	})

	t.Run("get not found", func(t *testing.T) {
		t.Parallel()
		store := setupTestStore(t)
		got, err := store.GetTargetGroup(t.Context(), "nonexistent")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("delete removes record", func(t *testing.T) {
		t.Parallel()
		store := setupTestStore(t)
		require.NoError(t, store.PutTargetGroup(t.Context(), newTestTG("del1", "tg-del1")))
		require.NoError(t, store.DeleteTargetGroup(t.Context(), "del1"))

		got, err := store.GetTargetGroup(t.Context(), "del1")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("delete idempotent", func(t *testing.T) {
		t.Parallel()
		store := setupTestStore(t)
		require.NoError(t, store.DeleteTargetGroup(t.Context(), "doesnt-exist"))
	})

	t.Run("list returns all", func(t *testing.T) {
		t.Parallel()
		store := setupTestStore(t)
		require.NoError(t, store.PutTargetGroup(t.Context(), newTestTG("a", "tg-a")))
		require.NoError(t, store.PutTargetGroup(t.Context(), newTestTG("b", "tg-b")))

		records, err := store.ListTargetGroups(t.Context())
		require.NoError(t, err)
		assert.Len(t, records, 2)
	})

	t.Run("list empty", func(t *testing.T) {
		t.Parallel()
		store := setupTestStore(t)
		records, err := store.ListTargetGroups(t.Context())
		require.NoError(t, err)
		assert.Empty(t, records)
	})
}

func TestListenerStoreLifecycle(t *testing.T) {
	t.Parallel()
	lbArn := "arn:aws:elasticloadbalancing:us-east-1:" + testAccountID + ":loadbalancer/app/test/lb1"

	t.Run("put and get", func(t *testing.T) {
		t.Parallel()
		store := setupTestStore(t)
		require.NoError(t, store.PutListener(t.Context(), newTestListener("getput1", lbArn)))

		got, err := store.GetListener(t.Context(), "getput1")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "getput1", got.ListenerID)
	})

	t.Run("get not found", func(t *testing.T) {
		t.Parallel()
		store := setupTestStore(t)
		got, err := store.GetListener(t.Context(), "nonexistent")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("delete removes record", func(t *testing.T) {
		t.Parallel()
		store := setupTestStore(t)
		require.NoError(t, store.PutListener(t.Context(), newTestListener("del1", lbArn)))
		require.NoError(t, store.DeleteListener(t.Context(), "del1"))

		got, err := store.GetListener(t.Context(), "del1")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("delete idempotent", func(t *testing.T) {
		t.Parallel()
		store := setupTestStore(t)
		require.NoError(t, store.DeleteListener(t.Context(), "doesnt-exist"))
	})

	t.Run("list returns all", func(t *testing.T) {
		t.Parallel()
		store := setupTestStore(t)
		require.NoError(t, store.PutListener(t.Context(), newTestListener("a", lbArn)))
		require.NoError(t, store.PutListener(t.Context(), newTestListener("b", lbArn)))

		records, err := store.ListListeners(t.Context())
		require.NoError(t, err)
		assert.Len(t, records, 2)
	})

	t.Run("list empty", func(t *testing.T) {
		t.Parallel()
		store := setupTestStore(t)
		records, err := store.ListListeners(t.Context())
		require.NoError(t, err)
		assert.Empty(t, records)
	})
}

// --- LB-specific lookups (no equivalent on TG/Listener) ---

func TestGetLoadBalancerByArn(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	store := setupTestStore(t)
	lb := newTestLB("name123", "find-by-name")
	require.NoError(t, store.PutLoadBalancer(t.Context(), lb))

	got, err := store.GetLoadBalancerByName(t.Context(), "find-by-name", testAccountID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, lb.LoadBalancerID, got.LoadBalancerID)
}

func TestPutLoadBalancer_Update(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	store := setupTestStore(t)
	tg := newTestTG("tgarn", "arn-tg")
	require.NoError(t, store.PutTargetGroup(t.Context(), tg))

	got, err := store.GetTargetGroupByArn(t.Context(), tg.TargetGroupArn)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, tg.Name, got.Name)
}

func TestGetTargetGroupByName(t *testing.T) {
	t.Parallel()
	store := setupTestStore(t)
	tg := newTestTG("tgname", "named-tg")
	require.NoError(t, store.PutTargetGroup(t.Context(), tg))

	got, err := store.GetTargetGroupByName(t.Context(), testAccountID, "named-tg", "vpc-test123")
	require.NoError(t, err)
	require.NotNil(t, got)

	// Wrong VPC should not find it
	got, err = store.GetTargetGroupByName(t.Context(), testAccountID, "named-tg", "vpc-other")
	require.NoError(t, err)
	assert.Nil(t, got)

	// Another account's group with the same name and VPC is not a match
	got, err = store.GetTargetGroupByName(t.Context(), "210987654321", "named-tg", "vpc-test123")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestTargetGroupWithTargets(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// A metadata edit through the CAS path must keep every retained field of a
// stored record, and must not add a realization key to a record without one.
func TestUpdateLoadBalancerPreservesRetainedRecord(t *testing.T) {
	t.Parallel()

	const legacyALB = `{"load_balancer_arn":"arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/ret-alb/rtalb1",` +
		`"load_balancer_id":"rtalb1","dns_name":"ret-alb-rtalb1.us-east-1.elb.spinifex.local","name":"ret-alb",` +
		`"scheme":"internal","type":"application","state":"active","vpc_id":"vpc-ret1","security_groups":["sg-ret1"],` +
		`"subnets":["subnet-ret1"],"availability_zones":[],"enis":["eni-alb1","eni-alb2"],"instance_id":"i-alb1",` +
		`"vpc_ip":"10.10.1.20","last_heartbeat":"2026-01-02T03:04:05Z","node_id":"node-1","ip_address_type":"ipv4",` +
		`"account_id":"123456789012","created_at":"2026-01-02T03:04:05Z"}`
	const legacyNLB = `{"load_balancer_arn":"arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/net/ret-nlb/rtnlb1",` +
		`"load_balancer_id":"rtnlb1","dns_name":"ret-nlb-rtnlb1.us-east-1.elb.spinifex.local","name":"ret-nlb",` +
		`"scheme":"internal","type":"network","state":"active","vpc_id":"vpc-ret2","security_groups":["sg-nlbmgd"],` +
		`"nlb_managed_sg_id":"sg-nlbmgd","subnets":["subnet-ret2"],"availability_zones":[],"enis":["eni-nlb1"],` +
		`"cross_account_enis":[{"eni_id":"eni-xacct1","eni_mac":"02:00:00:00:00:01","eni_ip":"10.20.1.5","subnet_id":"subnet-cust1","account_id":"210987654321"}],` +
		`"instance_id":"i-nlb1","vpc_ip":"10.10.2.20","last_heartbeat":"2026-01-02T03:04:05Z","node_id":"node-2",` +
		`"ip_address_type":"ipv4","account_id":"123456789012","created_at":"2026-01-02T03:04:05Z"}`
	const transitionalNLB = `{"load_balancer_arn":"arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/net/ret-ovn/rtovn1",` +
		`"load_balancer_id":"rtovn1","dns_name":"ret-ovn-rtovn1.us-east-1.elb.spinifex.local","name":"ret-ovn",` +
		`"scheme":"internal","type":"network","state":"active","vpc_id":"vpc-ret3","security_groups":["sg-ret3"],` +
		`"subnets":["subnet-ret3"],"availability_zones":[],"enis":["eni-ovn1"],"last_heartbeat":"2026-01-02T03:04:05Z",` +
		`"node_id":"","ip_address_type":"ipv4","account_id":"123456789012","created_at":"2026-01-02T03:04:05Z",` +
		`"realization":"ovn"}`

	retainedKeys := []string{
		"load_balancer_arn", "load_balancer_id", "dns_name", "name", "scheme", "type",
		"vpc_id", "security_groups", "nlb_managed_sg_id", "subnets", "enis", "cross_account_enis",
		"instance_id", "vpc_ip", "node_id", "account_id", "created_at",
	}

	cases := []struct {
		name            string
		id              string
		raw             string
		wantType        string
		wantRealization string
	}{
		{name: "legacy ALB", id: "rtalb1", raw: legacyALB, wantType: LoadBalancerTypeApplication},
		{name: "legacy NLB", id: "rtnlb1", raw: legacyNLB, wantType: LoadBalancerTypeNetwork},
		{name: "transitional OVN NLB", id: "rtovn1", raw: transitionalNLB, wantType: LoadBalancerTypeNetwork, wantRealization: "ovn"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := setupTestStore(t)
			ctx := t.Context()

			_, err := store.kv.Put(ctx, KeyPrefixLB+tc.id, []byte(tc.raw))
			require.NoError(t, err)
			var before map[string]any
			require.NoError(t, json.Unmarshal([]byte(tc.raw), &before))

			got, err := store.GetLoadBalancer(ctx, tc.id)
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tc.id, got.LoadBalancerID)
			assert.Equal(t, tc.wantType, got.Type)

			_, err = store.UpdateLoadBalancer(ctx, tc.id, func(rec *LoadBalancerRecord) (bool, error) {
				if rec.Tags == nil {
					rec.Tags = map[string]string{}
				}
				rec.Tags["team"] = "edge"
				return true, nil
			})
			require.NoError(t, err)

			entry, err := store.kv.Get(ctx, KeyPrefixLB+tc.id)
			require.NoError(t, err)
			var after map[string]any
			require.NoError(t, json.Unmarshal(entry.Value(), &after))

			assert.Equal(t, map[string]any{"team": "edge"}, after["tags"])
			for _, key := range retainedKeys {
				assert.Equal(t, before[key], after[key], "retained key %q changed", key)
			}
			if tc.wantRealization == "" {
				assert.NotContains(t, after, "realization")
			} else {
				assert.Equal(t, tc.wantRealization, after["realization"], "realization dropped by CAS rewrite")
			}
		})
	}
}
