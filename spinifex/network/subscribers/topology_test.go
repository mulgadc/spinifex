//test:in-package — the handlers under test are unexported methods on
// Subscriber, and the tests reuse newTestSubscriber and the respondResponse
// envelope, both of which are package-internal.

package subscribers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/types"
)

// runningSubscriber wires a subscriber to a live NATS server with every topic
// registered, so tests drive handlers the way vpcd does.
func runningSubscriber(t *testing.T) (*nats.Conn, *Subscriber, *mock.Client) {
	t.Helper()
	_, nc := testutil.StartTestNATS(t)
	sub, m := newTestSubscriber(t)
	subs, err := sub.Subscribe(nc)
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	})
	return nc, sub, m
}

// requestOK publishes evt and requires the handler's reply envelope to report
// success, so a handler that errors fails here rather than in a state assertion.
func requestOK(t *testing.T, nc *nats.Conn, topic string, evt any) {
	t.Helper()
	data, err := json.Marshal(evt)
	require.NoError(t, err)
	resp, err := nc.Request(topic, data, requestTimeout)
	require.NoError(t, err)
	var env respondResponse
	require.NoError(t, json.Unmarshal(resp.Data, &env))
	require.True(t, env.Success, "%s: %s", topic, env.Error)
}

// requestFails is requestOK's negative: the handler must reject the event and
// say so in the envelope.
func requestFails(t *testing.T, nc *nats.Conn, topic string, evt any) string {
	t.Helper()
	data, err := json.Marshal(evt)
	require.NoError(t, err)
	resp, err := nc.Request(topic, data, requestTimeout)
	require.NoError(t, err)
	var env respondResponse
	require.NoError(t, json.Unmarshal(resp.Data, &env))
	require.False(t, env.Success, "%s must reject the event", topic)
	return env.Error
}

// The mock reports a missing row as an error rather than a nil result, so
// absence is asserted against the list APIs.

func routerExists(t *testing.T, m *mock.Client, name string) bool {
	t.Helper()
	rows, err := m.ListLogicalRouters(context.Background())
	require.NoError(t, err)
	for _, r := range rows {
		if r.Name == name {
			return true
		}
	}
	return false
}

func switchExists(t *testing.T, m *mock.Client, name string) bool {
	t.Helper()
	rows, err := m.ListLogicalSwitches(context.Background())
	require.NoError(t, err)
	for _, s := range rows {
		if s.Name == name {
			return true
		}
	}
	return false
}

func switchPortExists(t *testing.T, m *mock.Client, name string) bool {
	t.Helper()
	rows, err := m.ListLogicalSwitchPorts(context.Background())
	require.NoError(t, err)
	for _, p := range rows {
		if p.Name == name {
			return true
		}
	}
	return false
}

func routerPortExists(t *testing.T, m *mock.Client, name string) bool {
	t.Helper()
	rows, err := m.ListLogicalRouterPorts(context.Background())
	require.NoError(t, err)
	for _, p := range rows {
		if p.Name == name {
			return true
		}
	}
	return false
}

// seedVPCWithIGW creates the router and attaches an IGW, the precondition for
// every egress-policy handler.
func seedVPCWithIGW(t *testing.T, sub *Subscriber, m *mock.Client, vpcID string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, m.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{
		Name:        topology.VPCRouter(vpcID),
		ExternalIDs: map[string]string{"spinifex:vpc_id": vpcID},
	}))
	require.NoError(t, sub.igw.AttachIGW(ctx, external.IGWSpec{VPCID: vpcID, InternetGatewayID: "igw-1"}))
}

func TestHandleVPC_CreateAndDelete(t *testing.T) {
	ctx := context.Background()
	nc, _, m := runningSubscriber(t)

	requestOK(t, nc, TopicVPCCreate, VPCEvent{VpcId: "vpc-1", CidrBlock: "10.0.0.0/16", VNI: 7})

	lr, err := m.GetLogicalRouter(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	require.NotNil(t, lr, "vpc.create must create the VPC logical router")
	assert.Equal(t, "vpc-1", lr.ExternalIDs["spinifex:vpc_id"])

	requestOK(t, nc, TopicVPCDelete, VPCEvent{VpcId: "vpc-1"})

	assert.False(t, routerExists(t, m, topology.VPCRouter("vpc-1")),
		"vpc.delete must remove the VPC logical router")
}

// A CIDR that will not parse must be rejected before any OVN write.
func TestHandleVPCCreate_InvalidCIDR(t *testing.T) {
	nc, _, m := runningSubscriber(t)

	requestFails(t, nc, TopicVPCCreate, VPCEvent{VpcId: "vpc-1", CidrBlock: "not-a-cidr"})

	assert.False(t, routerExists(t, m, topology.VPCRouter("vpc-1")),
		"invalid CIDR must not create a router")
}

// vpc.create and vpc.create-subnet have no ordering guarantee, so the subnet
// handler pre-ensures the router itself.
func TestHandleSubnetCreate_PreEnsuresVPCRouter(t *testing.T) {
	ctx := context.Background()
	nc, _, m := runningSubscriber(t)

	requestOK(t, nc, TopicSubnetCreate, SubnetEvent{
		SubnetId: "subnet-1", VpcId: "vpc-1", CidrBlock: "10.0.1.0/24",
	})

	lr, err := m.GetLogicalRouter(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	require.NotNil(t, lr, "subnet create must pre-ensure the VPC router")

	require.True(t, switchExists(t, m, topology.SubnetSwitch("subnet-1")),
		"subnet create must create the subnet switch")

	lrp, err := m.GetLogicalRouterPort(ctx, topology.SubnetRouterPort("subnet-1"))
	require.NoError(t, err)
	require.NotNil(t, lrp, "subnet create must attach the subnet to its router")
	assert.Contains(t, lrp.Networks, "10.0.1.1/24", "router port takes the subnet's first usable address")

	requestOK(t, nc, TopicSubnetDelete, SubnetEvent{
		SubnetId: "subnet-1", VpcId: "vpc-1", CidrBlock: "10.0.1.0/24",
	})

	assert.False(t, switchExists(t, m, topology.SubnetSwitch("subnet-1")),
		"subnet delete must remove the subnet switch")
}

func TestHandleSubnetCreate_InvalidCIDR(t *testing.T) {
	nc, _, m := runningSubscriber(t)

	requestFails(t, nc, TopicSubnetCreate, SubnetEvent{
		SubnetId: "subnet-1", VpcId: "vpc-1", CidrBlock: "10.0.1.0/33",
	})

	assert.False(t, switchExists(t, m, topology.SubnetSwitch("subnet-1")))
}

// delete-subnet tolerates a missing CIDR: the spec is keyed by ID, and the
// CIDR is only used to release the DHCP options row.
func TestHandleSubnetDelete_WithoutCIDR(t *testing.T) {
	nc, _, m := runningSubscriber(t)

	requestOK(t, nc, TopicSubnetCreate, SubnetEvent{
		SubnetId: "subnet-1", VpcId: "vpc-1", CidrBlock: "10.0.1.0/24",
	})
	requestOK(t, nc, TopicSubnetDelete, SubnetEvent{SubnetId: "subnet-1", VpcId: "vpc-1"})

	assert.False(t, switchExists(t, m, topology.SubnetSwitch("subnet-1")))
}

func TestHandlePort_CreateAndDelete(t *testing.T) {
	ctx := context.Background()
	nc, sub, m := runningSubscriber(t)

	requestOK(t, nc, TopicSubnetCreate, SubnetEvent{
		SubnetId: "subnet-1", VpcId: "vpc-1", CidrBlock: "10.0.1.0/24",
	})
	require.NoError(t, sub.topology.EnsureSGPortGroup(ctx, "sg-1"))

	requestOK(t, nc, TopicCreatePort, PortEvent{
		NetworkInterfaceId: "eni-1", SubnetId: "subnet-1", VpcId: "vpc-1",
		PrivateIpAddress: "10.0.1.10", MacAddress: "02:00:00:00:00:01",
		SecurityGroupIds: []string{"sg-1"},
	})

	lsp, err := m.GetLogicalSwitchPort(ctx, topology.Port("eni-1"))
	require.NoError(t, err)
	require.NotNil(t, lsp, "create-port must create the logical switch port")
	assert.Contains(t, lsp.Addresses, "02:00:00:00:00:01 10.0.1.10")

	pgs, err := m.ListPortGroupsForPort(ctx, topology.Port("eni-1"))
	require.NoError(t, err)
	assert.Contains(t, pgs, topology.SecurityGroupPortGroup("sg-1"),
		"create-port must join the port to its security group")

	requestOK(t, nc, TopicDeletePort, PortEvent{
		NetworkInterfaceId: "eni-1", SubnetId: "subnet-1", VpcId: "vpc-1",
	})

	assert.False(t, switchPortExists(t, m, topology.Port("eni-1")),
		"delete-port must remove the logical switch port")
}

// Address fields are parsed before the OVN write, so a malformed IP or MAC is
// reported rather than half-applied.
func TestHandleCreatePort_MalformedAddresses(t *testing.T) {
	nc, _, m := runningSubscriber(t)

	requestOK(t, nc, TopicSubnetCreate, SubnetEvent{
		SubnetId: "subnet-1", VpcId: "vpc-1", CidrBlock: "10.0.1.0/24",
	})

	requestFails(t, nc, TopicCreatePort, PortEvent{
		NetworkInterfaceId: "eni-bad-ip", SubnetId: "subnet-1", VpcId: "vpc-1",
		PrivateIpAddress: "999.0.0.1", MacAddress: "02:00:00:00:00:01",
	})
	requestFails(t, nc, TopicCreatePort, PortEvent{
		NetworkInterfaceId: "eni-bad-mac", SubnetId: "subnet-1", VpcId: "vpc-1",
		PrivateIpAddress: "10.0.1.10", MacAddress: "not-a-mac",
	})

	for _, name := range []string{"eni-bad-ip", "eni-bad-mac"} {
		assert.False(t, switchPortExists(t, m, topology.Port(name)), "%s must not be created", name)
	}
}

// update-port-sgs is declarative: the handler passes the full desired list and
// the manager diffs it, so a shrinking list drops memberships.
func TestHandleUpdatePortSGs_ReplacesMemberships(t *testing.T) {
	ctx := context.Background()
	nc, sub, m := runningSubscriber(t)

	requestOK(t, nc, TopicSubnetCreate, SubnetEvent{
		SubnetId: "subnet-1", VpcId: "vpc-1", CidrBlock: "10.0.1.0/24",
	})
	for _, sg := range []string{"sg-1", "sg-2"} {
		require.NoError(t, sub.topology.EnsureSGPortGroup(ctx, sg))
	}
	requestOK(t, nc, TopicCreatePort, PortEvent{
		NetworkInterfaceId: "eni-1", SubnetId: "subnet-1", VpcId: "vpc-1",
		PrivateIpAddress: "10.0.1.10", MacAddress: "02:00:00:00:00:01",
		SecurityGroupIds: []string{"sg-1"},
	})

	requestOK(t, nc, TopicUpdatePortSGs, UpdatePortSGsEvent{
		NetworkInterfaceId: "eni-1", PrivateIpAddress: "10.0.1.10",
		SecurityGroupIds: []string{"sg-2"},
	})

	pgs, err := m.ListPortGroupsForPort(ctx, topology.Port("eni-1"))
	require.NoError(t, err)
	assert.Contains(t, pgs, topology.SecurityGroupPortGroup("sg-2"))
	assert.NotContains(t, pgs, topology.SecurityGroupPortGroup("sg-1"),
		"a group absent from the event must be removed")
}

func TestHandleIGW_AttachAndDetach(t *testing.T) {
	nc, _, m := runningSubscriber(t)

	requestOK(t, nc, TopicVPCCreate, VPCEvent{VpcId: "vpc-1", CidrBlock: "10.0.0.0/16"})
	requestOK(t, nc, TopicIGWAttach, types.IGWEvent{VpcId: "vpc-1", InternetGatewayId: "igw-1"})

	require.True(t, routerPortExists(t, m, topology.GatewayRouterPort("vpc-1")),
		"igw-attach must create the gateway router port")

	requestOK(t, nc, TopicIGWDetach, types.IGWEvent{VpcId: "vpc-1", InternetGatewayId: "igw-1"})

	assert.False(t, routerPortExists(t, m, topology.GatewayRouterPort("vpc-1")),
		"igw-detach must remove the gateway router port")
}

func TestHandleNAT_AddAndDelete(t *testing.T) {
	ctx := context.Background()
	nc, sub, m := runningSubscriber(t)
	seedVPCWithIGW(t, sub, m, "vpc-1")

	evt := NATEvent{
		VpcId: "vpc-1", ExternalIP: "192.168.1.70", LogicalIP: "10.0.1.10",
		PortName: topology.Port("eni-1"), MAC: "02:00:00:00:00:01",
	}
	requestOK(t, nc, TopicAddNAT, evt)

	nat, err := m.FindNATByExternalIP(ctx, "dnat_and_snat", "192.168.1.70")
	require.NoError(t, err)
	require.NotNil(t, nat, "add-nat must install a dnat_and_snat rule")
	assert.Equal(t, "10.0.1.10", nat.LogicalIP)

	requestOK(t, nc, TopicDeleteNAT, evt)

	nat, err = m.FindNATByExternalIP(ctx, "dnat_and_snat", "192.168.1.70")
	require.NoError(t, err)
	assert.Nil(t, nat, "delete-nat must remove the rule")
}

// delete-igw-route must remove exactly the policy add-igw-route installed.
func TestHandleIGWRoute_DeleteRemovesPolicy(t *testing.T) {
	ctx := context.Background()
	nc, sub, m := runningSubscriber(t)
	seedVPCWithIGW(t, sub, m, "vpc-1")

	evt := IGWRouteEvent{
		VpcId: "vpc-1", SubnetId: "subnet-pub",
		DestinationCidr: "0.0.0.0/0", InternetGatewayId: "igw-1",
	}
	requestOK(t, nc, TopicAddIGWRoute, evt)

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	require.Len(t, policies, 1)

	requestOK(t, nc, TopicDeleteIGWRoute, evt)

	policies, err = m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	assert.Empty(t, policies, "delete-igw-route must remove the egress policy")
}

func TestHandleDeleteIGWRoute_InvalidCIDR(t *testing.T) {
	nc, sub, m := runningSubscriber(t)
	seedVPCWithIGW(t, sub, m, "vpc-1")

	requestFails(t, nc, TopicDeleteIGWRoute, IGWRouteEvent{
		VpcId: "vpc-1", SubnetId: "subnet-pub", DestinationCidr: "not-a-cidr",
	})
}

// Gating installs a DROP above both reroute priorities; ungating removes it.
func TestHandleSubnetEgress_GateAndUngate(t *testing.T) {
	ctx := context.Background()
	nc, sub, m := runningSubscriber(t)
	seedVPCWithIGW(t, sub, m, "vpc-1")

	gate := SubnetEgressGateEvent{VpcId: "vpc-1", SubnetId: "subnet-1", DestinationCidr: "0.0.0.0/0"}
	requestOK(t, nc, TopicGateSubnetEgress, gate)

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	require.Len(t, policies, 1)
	assert.Equal(t, policy.SubnetEgressPriorityDrop, policies[0].Priority)
	assert.Contains(t, policies[0].Match, topology.SubnetRouterPort("subnet-1"))

	requestOK(t, nc, TopicUngateSubnetEgress,
		SubnetEgressUngateEvent{VpcId: "vpc-1", SubnetId: "subnet-1", DestinationCidr: "0.0.0.0/0"})

	policies, err = m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	assert.Empty(t, policies, "ungate must remove the DROP policy")
}

func TestHandleSubnetEgress_InvalidCIDR(t *testing.T) {
	ctx := context.Background()
	nc, sub, m := runningSubscriber(t)
	seedVPCWithIGW(t, sub, m, "vpc-1")

	requestFails(t, nc, TopicGateSubnetEgress,
		SubnetEgressGateEvent{VpcId: "vpc-1", SubnetId: "subnet-1", DestinationCidr: "not-a-cidr"})
	requestFails(t, nc, TopicUngateSubnetEgress,
		SubnetEgressUngateEvent{VpcId: "vpc-1", SubnetId: "subnet-1", DestinationCidr: "not-a-cidr"})

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	assert.Empty(t, policies)
}
