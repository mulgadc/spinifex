package subscribers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
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
