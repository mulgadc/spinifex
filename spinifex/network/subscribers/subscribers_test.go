package subscribers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/mulgadc/spinifex/spinifex/testutil"
)

const requestTimeout = 2 * time.Second

// newTestSubscriber wires every manager against one mock OVN client.
func newTestSubscriber(t *testing.T) (*Subscriber, *mock.Client) {
	t.Helper()
	m := mock.New()
	_ = m.Connect(context.Background())

	topo := topology.NewLiveManager(m)
	sg := policy.NewSecurityGroupManager(m)
	nat, err := policy.NewNATManager(m, policy.NATModeDistributed)
	if err != nil {
		t.Fatalf("NewNATManager: %v", err)
	}
	routes := policy.NewRouteManager(m)
	igw, err := external.NewIGWManager(external.IGWManagerConfig{
		OVN:       m,
		Routes:    routes,
		NAT:       nat,
		Allocator: external.NewStaticRangeAllocator(m),
		Chassis:   []string{"hv1"},
		NATMode:   policy.NATModeDistributed,
	})
	if err != nil {
		t.Fatalf("NewIGWManager: %v", err)
	}
	eip, err := external.NewEIPManager(nat, nil)
	if err != nil {
		t.Fatalf("NewEIPManager: %v", err)
	}
	natgw, err := external.NewNATGWManager(nat)
	if err != nil {
		t.Fatalf("NewNATGWManager: %v", err)
	}
	s, err := New(Config{Topology: topo, SG: sg, EIP: eip, NATGW: natgw, IGW: igw})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, m
}

func TestNew_RejectsMissingDeps(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no topology", Config{SG: policy.NewSecurityGroupManager(mock.New())}},
		{"no sg", Config{Topology: topology.NewLiveManager(mock.New())}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := New(c.cfg); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestSubscribe_RegistersAllTopics(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	sub, _ := newTestSubscriber(t)

	subs, err := sub.Subscribe(nc)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	wantTopics := []string{
		TopicVPCCreate, TopicVPCDelete,
		TopicSubnetCreate, TopicSubnetDelete,
		TopicCreatePort, TopicDeletePort, TopicUpdatePortSGs,
		TopicIGWAttach, TopicIGWDetach,
		TopicAddNAT, TopicDeleteNAT,
		TopicAddNATGateway, TopicDeleteNATGateway,
		TopicAddIGWRoute, TopicDeleteIGWRoute,
		TopicCreateSG, TopicDeleteSG, TopicUpdateSG,
	}
	if len(subs) != len(wantTopics) {
		t.Fatalf("subscription count: got %d, want %d", len(subs), len(wantTopics))
	}
	got := map[string]bool{}
	for _, s := range subs {
		got[s.Subject] = true
	}
	for _, topic := range wantTopics {
		if !got[topic] {
			t.Errorf("missing subscription for topic %q", topic)
		}
	}
}

// TestHandleAddNATGateway_InstallsPerSubnetEgress drives the regression: the
// subscriber must install a Logical_Router_Policy at NATGW priority alongside
// the SNAT rule, otherwise private-subnet packets have no route off the LR
// and NAT GW egress fails (Phase 8d). Verifies the policy is installed
// against the IGW's gateway port, with the inport scoped to the private
// subnet, after a vpc.add-nat-gateway event.
func TestHandleAddNATGateway_InstallsPerSubnetEgress(t *testing.T) {
	ctx := context.Background()
	_, nc := testutil.StartTestNATS(t)
	sub, m := newTestSubscriber(t)
	subs, err := sub.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Bootstrap router + IGW so the gateway port + wan nexthop exist.
	require.NoError(t, m.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{
		Name:        topology.VPCRouter("vpc-1"),
		ExternalIDs: map[string]string{"spinifex:vpc_id": "vpc-1"},
	}))
	require.NoError(t, sub.igw.AttachIGW(ctx, external.IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))

	evt := NATGatewayEvent{
		VpcId:           "vpc-1",
		NatGatewayId:    "nat-1",
		PublicIp:        "192.168.1.50",
		SubnetCidr:      "10.0.11.0/24",
		SubnetId:        "subnet-priv",
		DestinationCidr: "0.0.0.0/0",
	}
	data, err := json.Marshal(evt)
	require.NoError(t, err)
	require.NoError(t, nc.Publish(TopicAddNATGateway, data))

	require.Eventually(t, func() bool {
		policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
		if err != nil || len(policies) != 1 {
			return false
		}
		return policies[0].Priority == policy.SubnetEgressPriorityNATGW
	}, 2*time.Second, 20*time.Millisecond, "NATGW egress policy must be installed")

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	require.Len(t, policies, 1)
	assert.Contains(t, policies[0].Match, topology.SubnetRouterPort("subnet-priv"))
	assert.Equal(t, topology.GatewayRouterPort("vpc-1"), policies[0].ExternalIDs["spinifex:output_port"])

	// Delete event must remove the policy.
	require.NoError(t, nc.Publish(TopicDeleteNATGateway, data))
	require.Eventually(t, func() bool {
		policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
		return err == nil && len(policies) == 0
	}, 2*time.Second, 20*time.Millisecond, "NATGW egress policy must be removed on delete event")
}

// TestHandleAddNATGateway_InvalidCIDRSkipsPolicy: the SNAT install must still
// happen, but a malformed DestinationCidr must not install any LR policy.
func TestHandleAddNATGateway_InvalidCIDRSkipsPolicy(t *testing.T) {
	ctx := context.Background()
	_, nc := testutil.StartTestNATS(t)
	sub, m := newTestSubscriber(t)
	subs, err := sub.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	require.NoError(t, m.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{
		Name:        topology.VPCRouter("vpc-1"),
		ExternalIDs: map[string]string{"spinifex:vpc_id": "vpc-1"},
	}))
	require.NoError(t, sub.igw.AttachIGW(ctx, external.IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))

	evt := NATGatewayEvent{
		VpcId:           "vpc-1",
		NatGatewayId:    "nat-1",
		PublicIp:        "192.168.1.50",
		SubnetCidr:      "10.0.11.0/24",
		SubnetId:        "subnet-priv",
		DestinationCidr: "not-a-cidr",
	}
	data, err := json.Marshal(evt)
	require.NoError(t, err)
	require.NoError(t, nc.Publish(TopicAddNATGateway, data))

	// SNAT lands eventually.
	require.Eventually(t, func() bool {
		nat, err := m.FindNATByExternalIP(ctx, "snat", "192.168.1.50")
		return err == nil && nat != nil
	}, time.Second, 20*time.Millisecond)

	// No policy installed — invalid CIDR short-circuits before EnsureNATGatewaySubnetEgress.
	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	assert.Empty(t, policies)
}

// TestHandleDeleteNATGateway_InvalidCIDRStillDetaches: malformed CIDR on the
// delete event must not block the SNAT teardown.
func TestHandleDeleteNATGateway_InvalidCIDRStillDetaches(t *testing.T) {
	ctx := context.Background()
	_, nc := testutil.StartTestNATS(t)
	sub, m := newTestSubscriber(t)
	subs, err := sub.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	require.NoError(t, m.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{
		Name:        topology.VPCRouter("vpc-1"),
		ExternalIDs: map[string]string{"spinifex:vpc_id": "vpc-1"},
	}))
	require.NoError(t, sub.igw.AttachIGW(ctx, external.IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))

	addEvt := NATGatewayEvent{
		VpcId: "vpc-1", NatGatewayId: "nat-1", PublicIp: "192.168.1.50",
		SubnetCidr: "10.0.11.0/24", SubnetId: "subnet-priv", DestinationCidr: "0.0.0.0/0",
	}
	addData, err := json.Marshal(addEvt)
	require.NoError(t, err)
	require.NoError(t, nc.Publish(TopicAddNATGateway, addData))
	require.Eventually(t, func() bool {
		nat, err := m.FindNATByExternalIP(ctx, "snat", "192.168.1.50")
		return err == nil && nat != nil
	}, time.Second, 20*time.Millisecond)

	delEvt := addEvt
	delEvt.DestinationCidr = "not-a-cidr"
	delData, err := json.Marshal(delEvt)
	require.NoError(t, err)
	require.NoError(t, nc.Publish(TopicDeleteNATGateway, delData))

	require.Eventually(t, func() bool {
		nat, err := m.FindNATByExternalIP(ctx, "snat", "192.168.1.50")
		return err == nil && nat == nil
	}, time.Second, 20*time.Millisecond, "SNAT must be removed even when CIDR is invalid")
}

// TestHandleAddNATGateway_NoSubnetIDSkipsPolicy: legacy events without a
// subnet (or AWS gateway types that don't bind to a specific subnet) must
// install the SNAT rule and short-circuit before any policy work.
func TestHandleAddNATGateway_NoSubnetIDSkipsPolicy(t *testing.T) {
	ctx := context.Background()
	_, nc := testutil.StartTestNATS(t)
	sub, m := newTestSubscriber(t)
	subs, err := sub.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	require.NoError(t, m.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{
		Name:        topology.VPCRouter("vpc-1"),
		ExternalIDs: map[string]string{"spinifex:vpc_id": "vpc-1"},
	}))
	require.NoError(t, sub.igw.AttachIGW(ctx, external.IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))

	evt := NATGatewayEvent{
		VpcId: "vpc-1", NatGatewayId: "nat-1", PublicIp: "192.168.1.50",
		SubnetCidr: "10.0.11.0/24", // SubnetId omitted
	}
	data, err := json.Marshal(evt)
	require.NoError(t, err)
	require.NoError(t, nc.Publish(TopicAddNATGateway, data))

	require.Eventually(t, func() bool {
		nat, err := m.FindNATByExternalIP(ctx, "snat", "192.168.1.50")
		return err == nil && nat != nil
	}, time.Second, 20*time.Millisecond)

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	assert.Empty(t, policies, "missing SubnetId must skip Logical_Router_Policy install")
}

// TestHandleAddNATGateway_DefaultsDestinationCidr verifies the empty-CIDR
// fallback to 0.0.0.0/0 — older publishers don't set DestinationCidr.
func TestHandleAddNATGateway_DefaultsDestinationCidr(t *testing.T) {
	ctx := context.Background()
	_, nc := testutil.StartTestNATS(t)
	sub, m := newTestSubscriber(t)
	subs, err := sub.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	require.NoError(t, m.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{
		Name:        topology.VPCRouter("vpc-1"),
		ExternalIDs: map[string]string{"spinifex:vpc_id": "vpc-1"},
	}))
	require.NoError(t, sub.igw.AttachIGW(ctx, external.IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))

	evt := NATGatewayEvent{
		VpcId: "vpc-1", NatGatewayId: "nat-1", PublicIp: "192.168.1.50",
		SubnetCidr: "10.0.11.0/24", SubnetId: "subnet-priv", // DestinationCidr empty
	}
	data, err := json.Marshal(evt)
	require.NoError(t, err)
	require.NoError(t, nc.Publish(TopicAddNATGateway, data))

	require.Eventually(t, func() bool {
		policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
		return err == nil && len(policies) == 1
	}, time.Second, 20*time.Millisecond)

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	require.Len(t, policies, 1)
	assert.Contains(t, policies[0].Match, "ip4.dst == 0.0.0.0/0")
}

// TestHandleAddIGWRoute_InstallsPolicy: full end-to-end add-igw-route flow.
func TestHandleAddIGWRoute_InstallsPolicy(t *testing.T) {
	ctx := context.Background()
	_, nc := testutil.StartTestNATS(t)
	sub, m := newTestSubscriber(t)
	subs, err := sub.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	require.NoError(t, m.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{
		Name:        topology.VPCRouter("vpc-1"),
		ExternalIDs: map[string]string{"spinifex:vpc_id": "vpc-1"},
	}))
	require.NoError(t, sub.igw.AttachIGW(ctx, external.IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))

	evt := IGWRouteEvent{
		VpcId: "vpc-1", SubnetId: "subnet-pub", DestinationCidr: "0.0.0.0/0", InternetGatewayId: "igw-1",
	}
	data, err := json.Marshal(evt)
	require.NoError(t, err)
	resp, err := nc.Request(TopicAddIGWRoute, data, requestTimeout)
	require.NoError(t, err)
	var env respondResponse
	require.NoError(t, json.Unmarshal(resp.Data, &env))
	assert.True(t, env.Success)

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	require.Len(t, policies, 1)
	assert.Equal(t, policy.SubnetEgressPriorityIGW, policies[0].Priority)
}

// TestHandleDeleteNATGateway_MissingRouterStillReturns covers the failure
// path where the underlying router has been torn down already: the handler
// must log + return rather than panic. Exercises both the
// RemoveNATGatewaySubnetEgress error branch and the DetachNATGateway error
// branch, and the empty-DestinationCidr default of 0.0.0.0/0.
func TestHandleDeleteNATGateway_MissingRouterStillReturns(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	sub, _ := newTestSubscriber(t)
	subs, err := sub.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	evt := NATGatewayEvent{
		VpcId:        "vpc-gone",
		NatGatewayId: "nat-1",
		PublicIp:     "192.168.1.50",
		SubnetCidr:   "10.0.11.0/24",
		SubnetId:     "subnet-priv",
		// DestinationCidr intentionally empty → handler must default to 0.0.0.0/0.
	}
	data, err := json.Marshal(evt)
	require.NoError(t, err)
	require.NoError(t, nc.Publish(TopicDeleteNATGateway, data))
	time.Sleep(50 * time.Millisecond)
}

// TestHandleNATGateway_BadJSON: malformed payloads on add/delete must not
// panic the subscriber. Both topics are fire-and-forget (no reply).
func TestHandleNATGateway_BadJSON(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	sub, _ := newTestSubscriber(t)
	subs, err := sub.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	require.NoError(t, nc.Publish(TopicAddNATGateway, []byte("not json")))
	require.NoError(t, nc.Publish(TopicDeleteNATGateway, []byte("not json")))
	// Give handlers a moment to run — nothing to assert except no panic.
	time.Sleep(50 * time.Millisecond)
}

// TestHandleIGWRoute_BadJSON: bad payload on add-igw-route must return an
// error envelope; the subscriber pattern uses respond() for request/reply.
func TestHandleIGWRoute_BadJSON(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	sub, _ := newTestSubscriber(t)
	subs, err := sub.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	for _, topic := range []string{TopicAddIGWRoute, TopicDeleteIGWRoute} {
		resp, err := nc.Request(topic, []byte("not json"), requestTimeout)
		require.NoError(t, err)
		var env respondResponse
		require.NoError(t, json.Unmarshal(resp.Data, &env))
		assert.False(t, env.Success, "topic %s must return success=false on bad JSON", topic)
	}
}

// TestHandleAddIGWRoute_InvalidCIDR: malformed CIDR must be rejected via the
// reply envelope and must not install any policy.
func TestHandleAddIGWRoute_InvalidCIDR(t *testing.T) {
	ctx := context.Background()
	_, nc := testutil.StartTestNATS(t)
	sub, m := newTestSubscriber(t)
	subs, err := sub.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	require.NoError(t, m.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{
		Name:        topology.VPCRouter("vpc-1"),
		ExternalIDs: map[string]string{"spinifex:vpc_id": "vpc-1"},
	}))
	require.NoError(t, sub.igw.AttachIGW(ctx, external.IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))

	evt := IGWRouteEvent{
		VpcId: "vpc-1", SubnetId: "subnet-pub", DestinationCidr: "not-a-cidr", InternetGatewayId: "igw-1",
	}
	data, err := json.Marshal(evt)
	require.NoError(t, err)
	resp, err := nc.Request(TopicAddIGWRoute, data, requestTimeout)
	require.NoError(t, err)
	var env respondResponse
	require.NoError(t, json.Unmarshal(resp.Data, &env))
	assert.False(t, env.Success)

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	assert.Empty(t, policies)
}

// Every request/reply topic must return a structured error envelope on bad
// JSON. Fire-and-forget topics (NAT gateway) just must not panic.
func TestBadJSON_AllRequestReplies(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	sub, _ := newTestSubscriber(t)
	subs, err := sub.Subscribe(nc)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	topics := []string{
		TopicVPCCreate, TopicVPCDelete,
		TopicSubnetCreate, TopicSubnetDelete,
		TopicCreatePort, TopicDeletePort, TopicUpdatePortSGs,
		TopicIGWAttach, TopicIGWDetach,
		TopicAddNAT, TopicDeleteNAT,
		TopicCreateSG, TopicDeleteSG, TopicUpdateSG,
	}
	for _, topic := range topics {
		t.Run(topic, func(t *testing.T) {
			resp, err := nc.Request(topic, []byte("not json"), requestTimeout)
			if err != nil {
				t.Fatalf("request %s: %v", topic, err)
			}
			var env respondResponse
			if err := json.Unmarshal(resp.Data, &env); err != nil {
				t.Fatalf("unmarshal envelope: %v", err)
			}
			if env.Success {
				t.Errorf("topic %s: expected success=false on bad JSON, got success=true", topic)
			}
		})
	}
}
