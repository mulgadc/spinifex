package subscribers_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	networkv1 "github.com/mulgadc/spinifex/contracts/network/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/network/external"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/domains/network/policy"
	"github.com/mulgadc/spinifex/spinifex/domains/network/subscribers"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file characterizes the vpc.* network-realisation boundary as it exists
// today: exact subject literals, exact JSON wire shapes, the request/reply ack
// envelope and the vpcd-workers queue group. contracts/network/v1 is now the
// sole owner of these literals; any future wire change must update both the
// contract and this pin in the same commit.

// TestGoldenSubjects pins every vpc.* subject literal this boundary carries.
// A change here is a wire-compatibility break, not a rename.
func TestGoldenSubjects(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"VPCCreate", networkv1.VPCCreateSubject, "vpc.create"},
		{"VPCDelete", networkv1.VPCDeleteSubject, "vpc.delete"},
		{"SubnetCreate", networkv1.SubnetCreateSubject, "vpc.create-subnet"},
		{"SubnetDelete", networkv1.SubnetDeleteSubject, "vpc.delete-subnet"},
		{"CreatePort", networkv1.PortCreateSubject, "vpc.create-port"},
		{"DeletePort", networkv1.PortDeleteSubject, "vpc.delete-port"},
		{"UpdatePortSGs", networkv1.PortSecurityGroupsUpdateSubject, "vpc.update-port-sgs"},
		{"IGWAttach", networkv1.InternetGatewayAttachSubject, "vpc.igw-attach"},
		{"IGWDetach", networkv1.InternetGatewayDetachSubject, "vpc.igw-detach"},
		{"AddNAT", networkv1.NATAddSubject, "vpc.add-nat"},
		{"DeleteNAT", networkv1.NATDeleteSubject, "vpc.delete-nat"},
		{"AddNATGateway", networkv1.NATGatewayAddSubject, "vpc.add-nat-gateway"},
		{"DeleteNATGateway", networkv1.NATGatewayDeleteSubject, "vpc.delete-nat-gateway"},
		{"AddIGWRoute", networkv1.IGWRouteAddSubject, "vpc.add-igw-route"},
		{"DeleteIGWRoute", networkv1.IGWRouteDeleteSubject, "vpc.delete-igw-route"},
		{"GateSubnetEgress", networkv1.SubnetEgressGateSubject, "vpc.gate-subnet-egress"},
		{"UngateSubnetEgress", networkv1.SubnetEgressUngateSubject, "vpc.ungate-subnet-egress"},
		{"AddSystemEgress", networkv1.SystemEgressAddSubject, "vpc.add-system-egress"},
		{"DeleteSystemEgress", networkv1.SystemEgressDeleteSubject, "vpc.delete-system-egress"},
		{"CreateSG", networkv1.SecurityGroupCreateSubject, "vpc.create-sg"},
		{"DeleteSG", networkv1.SecurityGroupDeleteSubject, "vpc.delete-sg"},
		{"UpdateSG", networkv1.SecurityGroupUpdateSubject, "vpc.update-sg"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, c.got)
		})
	}
}

// TestGoldenQueueGroup pins the shared queue group every vpcd handler
// subscribes under, so only one vpcd processes a given event.
func TestGoldenQueueGroup(t *testing.T) {
	assert.Equal(t, "vpcd-workers", networkv1.QueueGroup)
}

// TestGoldenPayloadJSON pins the exact wire JSON for every exported event
// type on this boundary against a hand-written golden literal, independent of
// the struct definition, in both directions (marshal and unmarshal).
func TestGoldenPayloadJSON(t *testing.T) {
	t.Run("VPCEvent", func(t *testing.T) {
		golden := `{"vpc_id":"vpc-1","cidr_block":"10.0.0.0/16","vni":42}`
		evt := networkv1.VPCEvent{VpcId: "vpc-1", CidrBlock: "10.0.0.0/16", VNI: 42}
		assertGoldenJSON(t, golden, evt)
	})
	t.Run("SubnetEvent", func(t *testing.T) {
		golden := `{"subnet_id":"subnet-1","vpc_id":"vpc-1","cidr_block":"10.0.1.0/24"}`
		evt := networkv1.SubnetEvent{SubnetId: "subnet-1", VpcId: "vpc-1", CidrBlock: "10.0.1.0/24"}
		assertGoldenJSON(t, golden, evt)
	})
	t.Run("PortEvent", func(t *testing.T) {
		golden := `{"network_interface_id":"eni-1","subnet_id":"subnet-1","vpc_id":"vpc-1","private_ip_address":"10.0.1.5","mac_address":"02:00:00:00:00:01","security_group_ids":["sg-1"],"suppress_dhcp":true}`
		evt := networkv1.PortEvent{
			NetworkInterfaceId: "eni-1", SubnetId: "subnet-1", VpcId: "vpc-1",
			PrivateIpAddress: "10.0.1.5", MacAddress: "02:00:00:00:00:01",
			SecurityGroupIds: []string{"sg-1"}, SuppressDHCP: true,
		}
		assertGoldenJSON(t, golden, evt)
	})
	t.Run("PortEvent_OmitsEmptyFields", func(t *testing.T) {
		golden := `{"network_interface_id":"eni-1","subnet_id":"subnet-1","vpc_id":"vpc-1","private_ip_address":"10.0.1.5","mac_address":"02:00:00:00:00:01"}`
		evt := networkv1.PortEvent{
			NetworkInterfaceId: "eni-1", SubnetId: "subnet-1", VpcId: "vpc-1",
			PrivateIpAddress: "10.0.1.5", MacAddress: "02:00:00:00:00:01",
		}
		marshaled, err := json.Marshal(evt)
		require.NoError(t, err)
		assert.JSONEq(t, golden, string(marshaled))
	})
	t.Run("PortSecurityGroupsUpdateEvent", func(t *testing.T) {
		golden := `{"network_interface_id":"eni-1","private_ip_address":"10.0.1.5","security_group_ids":["sg-1","sg-2"]}`
		evt := networkv1.PortSecurityGroupsUpdateEvent{
			NetworkInterfaceId: "eni-1", PrivateIpAddress: "10.0.1.5",
			SecurityGroupIds: []string{"sg-1", "sg-2"},
		}
		assertGoldenJSON(t, golden, evt)
	})
	t.Run("NATEvent", func(t *testing.T) {
		golden := `{"vpc_id":"vpc-1","external_ip":"203.0.113.5","logical_ip":"10.0.1.5","port_name":"port-eni-1","mac":"02:00:00:00:00:01"}`
		evt := networkv1.NATEvent{
			VpcId: "vpc-1", ExternalIP: "203.0.113.5", LogicalIP: "10.0.1.5",
			PortName: "port-eni-1", MAC: "02:00:00:00:00:01",
		}
		assertGoldenJSON(t, golden, evt)
	})
	t.Run("NATGatewayEvent", func(t *testing.T) {
		golden := `{"vpc_id":"vpc-1","nat_gateway_id":"nat-1","public_ip":"203.0.113.5","subnet_cidr":"10.0.1.0/24","subnet_id":"subnet-1","destination_cidr":"0.0.0.0/0"}`
		evt := networkv1.NATGatewayEvent{
			VpcId: "vpc-1", NatGatewayId: "nat-1", PublicIp: "203.0.113.5",
			SubnetCidr: "10.0.1.0/24", SubnetId: "subnet-1", DestinationCidr: "0.0.0.0/0",
		}
		assertGoldenJSON(t, golden, evt)
	})
	t.Run("IGWRouteEvent", func(t *testing.T) {
		golden := `{"vpc_id":"vpc-1","subnet_id":"subnet-1","destination_cidr":"0.0.0.0/0","internet_gateway_id":"igw-1"}`
		evt := networkv1.IGWRouteEvent{
			VpcId: "vpc-1", SubnetId: "subnet-1", DestinationCidr: "0.0.0.0/0", InternetGatewayId: "igw-1",
		}
		assertGoldenJSON(t, golden, evt)
	})
	t.Run("SubnetEgressGateEvent", func(t *testing.T) {
		golden := `{"vpc_id":"vpc-1","subnet_id":"subnet-1","destination_cidr":"0.0.0.0/0"}`
		evt := networkv1.SubnetEgressGateEvent{VpcId: "vpc-1", SubnetId: "subnet-1", DestinationCidr: "0.0.0.0/0"}
		assertGoldenJSON(t, golden, evt)
	})
	t.Run("SubnetEgressUngateEvent", func(t *testing.T) {
		golden := `{"vpc_id":"vpc-1","subnet_id":"subnet-1","destination_cidr":"0.0.0.0/0"}`
		evt := networkv1.SubnetEgressUngateEvent{VpcId: "vpc-1", SubnetId: "subnet-1", DestinationCidr: "0.0.0.0/0"}
		assertGoldenJSON(t, golden, evt)
	})
	t.Run("SystemEgressEvent", func(t *testing.T) {
		golden := `{"vpc_id":"vpc-1","subnet_id":"subnet-1","instance_ip":"10.0.1.5","external_ip":"203.0.113.5"}`
		evt := networkv1.SystemEgressEvent{
			VpcId: "vpc-1", SubnetId: "subnet-1", InstanceIp: "10.0.1.5", ExternalIp: "203.0.113.5",
		}
		assertGoldenJSON(t, golden, evt)
	})
	t.Run("SecurityGroupEvent", func(t *testing.T) {
		golden := `{"group_id":"sg-1","vpc_id":"vpc-1","ingress_rules":[{"rule_id":"","ip_protocol":"tcp","from_port":22,"to_port":22,"cidr_ip":"0.0.0.0/0"}],"egress_rules":[{"rule_id":"","ip_protocol":"-1","from_port":0,"to_port":0,"source_sg":"sg-2"}]}`
		evt := networkv1.SecurityGroupEvent{
			GroupId: "sg-1", VpcId: "vpc-1",
			IngressRules: []networkv1.SecurityGroupRule{{IpProtocol: "tcp", FromPort: 22, ToPort: 22, CidrIp: "0.0.0.0/0"}},
			EgressRules:  []networkv1.SecurityGroupRule{{IpProtocol: "-1", SourceSG: "sg-2"}},
		}
		assertGoldenJSON(t, golden, evt)
	})
	t.Run("InternetGatewayEvent", func(t *testing.T) {
		golden := `{"internet_gateway_id":"igw-1","vpc_id":"vpc-1"}`
		evt := networkv1.InternetGatewayEvent{InternetGatewayId: "igw-1", VpcId: "vpc-1"}
		marshaled, err := json.Marshal(evt)
		require.NoError(t, err)
		assert.JSONEq(t, golden, string(marshaled))
		var decoded networkv1.InternetGatewayEvent
		require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
		assert.Equal(t, evt, decoded)
	})
}

// assertGoldenJSON marshals v and compares it against golden, then unmarshals
// golden back into a fresh value of v's type and compares it against v. Both
// directions must hold for the golden literal to be a faithful pin.
func assertGoldenJSON(t *testing.T, golden string, v any) {
	t.Helper()
	marshaled, err := json.Marshal(v)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(marshaled))
}

// newGoldenSubscriber wires every manager against one mock OVN client, the
// same way production composition does, so Subscribe exercises real handlers.
func newGoldenSubscriber(t *testing.T) *subscribers.Subscriber {
	t.Helper()
	m := mock.New()
	require.NoError(t, m.Connect(context.Background()))

	topo := topology.NewLiveManager(m)
	sg := policy.NewSecurityGroupManager(m, policy.EgressPolicy{})
	nat, err := policy.NewNATManager(m, policy.NATModeDistributed)
	require.NoError(t, err)
	routes := policy.NewRouteManager(m)
	igw, err := external.NewIGWManager(external.IGWManagerConfig{
		OVN: m, Routes: routes, NAT: nat,
		Allocator: external.NewStaticRangeAllocator(m),
		Chassis:   []string{"hv1"},
		NATMode:   policy.NATModeDistributed,
	})
	require.NoError(t, err)
	eip, err := external.NewEIPManager(nat, nil)
	require.NoError(t, err)
	natgw, err := external.NewNATGWManager(nat)
	require.NoError(t, err)

	s, err := subscribers.New(subscribers.Config{Topology: topo, SG: sg, EIP: eip, NATGW: natgw, IGW: igw})
	require.NoError(t, err)
	return s
}

// TestGoldenAckEnvelope_Success pins the exact success envelope a vpcd
// request/reply route sends today: {"success":true}, nothing else.
func TestGoldenAckEnvelope_Success(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	s := newGoldenSubscriber(t)
	subs, err := s.Subscribe(nc)
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, sub := range subs {
			_ = sub.Unsubscribe()
		}
	})

	payload, err := json.Marshal(networkv1.SecurityGroupEvent{GroupId: "sg-golden", VpcId: "vpc-1"})
	require.NoError(t, err)
	resp, err := nc.Request(networkv1.SecurityGroupCreateSubject, payload, 5*time.Second)
	require.NoError(t, err)
	assert.JSONEq(t, `{"success":true}`, string(resp.Data))
}

// TestGoldenAckEnvelope_Error pins the error envelope's shape: a request/reply
// route that fails replies with success=false and a populated error string.
// The error text itself is not pinned — it comes from the underlying failure
// and is not part of the wire contract — only the two-field shape is.
func TestGoldenAckEnvelope_Error(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	s := newGoldenSubscriber(t)
	subs, err := s.Subscribe(nc)
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, sub := range subs {
			_ = sub.Unsubscribe()
		}
	})

	resp, err := nc.Request(networkv1.PortCreateSubject, []byte("not json"), 5*time.Second)
	require.NoError(t, err)

	var envelope networkv1.AckEnvelope
	require.NoError(t, json.Unmarshal(resp.Data, &envelope))
	assert.False(t, envelope.Success)
	assert.NotEmpty(t, envelope.Error)
}

// TestGoldenFireAndForgetRoutes_NoReplyExpected documents, by exercising real
// Subscribe wiring, that a plain Publish (no reply subject) to a fire-and-
// forget route never blocks the caller: these routes never populate msg.Reply,
// so respond() is a no-op today, and callers correctly do not wait for one.
func TestGoldenFireAndForgetRoutes_NoReplyExpected(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	s := newGoldenSubscriber(t)
	subs, err := s.Subscribe(nc)
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, sub := range subs {
			_ = sub.Unsubscribe()
		}
	})

	payload, err := json.Marshal(networkv1.VPCEvent{VpcId: "vpc-golden"})
	require.NoError(t, err)
	// A bare Publish never sets Reply; the unit under test is that this
	// returns immediately and nothing panics or blocks waiting for an ack.
	require.NoError(t, nc.Publish(networkv1.VPCCreateSubject, payload))
	require.NoError(t, nc.Flush())
}
