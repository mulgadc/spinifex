//go:build e2e

package single

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// peeringGuest is one VPC with a public subnet, an IGW default route on its
// main table and one instance reachable over SSH and ICMP.
type peeringGuest struct {
	vpcID, cidr, subnetID, rtbID, igwID string
	instanceID, publicIP, privateIP     string
	tgt                                 harness.SSHTarget
}

func newPeeringGuest(t *testing.T, fix *Fixture, cidr, subnetCIDR, ami, instType, keyName, keyPath string) *peeringGuest {
	t.Helper()
	c := fix.AWS
	g := &peeringGuest{cidr: cidr}
	g.vpcID = harness.CreateVPC(t, c, cidr)
	t.Cleanup(func() { harness.DeleteVPC(t, c, g.vpcID) })
	g.subnetID = harness.CreateSubnet(t, c, g.vpcID, subnetCIDR)
	t.Cleanup(func() { harness.DeleteSubnet(t, c, g.subnetID) })
	_, err := c.EC2.ModifySubnetAttribute(&ec2.ModifySubnetAttributeInput{
		SubnetId: aws.String(g.subnetID), MapPublicIpOnLaunch: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	})
	require.NoError(t, err, "modify-subnet-attribute")

	igw, err := c.EC2.CreateInternetGateway(&ec2.CreateInternetGatewayInput{})
	require.NoError(t, err, "create-internet-gateway")
	g.igwID = aws.StringValue(igw.InternetGateway.InternetGatewayId)
	t.Cleanup(func() {
		_, _ = c.EC2.DetachInternetGateway(&ec2.DetachInternetGatewayInput{InternetGatewayId: aws.String(g.igwID), VpcId: aws.String(g.vpcID)})
		_, _ = c.EC2.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{InternetGatewayId: aws.String(g.igwID)})
	})
	_, err = c.EC2.AttachInternetGateway(&ec2.AttachInternetGatewayInput{InternetGatewayId: aws.String(g.igwID), VpcId: aws.String(g.vpcID)})
	require.NoError(t, err, "attach-internet-gateway")
	g.rtbID = mainRouteTableID(t, c, g.vpcID)
	_, err = c.EC2.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId: aws.String(g.rtbID), DestinationCidrBlock: aws.String("0.0.0.0/0"), GatewayId: aws.String(g.igwID),
	})
	require.NoError(t, err, "create-route 0.0.0.0/0 -> igw")

	sg := defaultSecurityGroup(t, c, g.vpcID)
	harness.AuthorizeSSHIngress(t, c, sg)
	harness.AuthorizeICMPIngress(t, c, sg)

	g.instanceID = launchBaselineInstance(t, fix, ami, instType, keyName, g.subnetID, []string{sg})
	g.publicIP = instancePublicIP(t, fix, g.instanceID)
	g.privateIP = instancePrivateIP(t, fix, g.instanceID)
	g.tgt = harness.SSHTarget{User: "ubuntu", Host: g.publicIP, Port: 22, KeyPath: keyPath}
	require.Truef(t, sshHealth.TryReady(t, g.publicIP, 22, keyPath, harness.SSHReadyBudget),
		"SSH to %s (%s) never became ready", g.instanceID, g.publicIP)
	harness.Detail(t, "vpc", g.vpcID, "instance", g.instanceID, "private_ip", g.privateIP)
	return g
}

func defaultSecurityGroup(t *testing.T, c *harness.AWSClient, vpcID string) string {
	t.Helper()
	out, err := c.EC2.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{Filters: []*ec2.Filter{
		{Name: aws.String("vpc-id"), Values: []*string{aws.String(vpcID)}},
		{Name: aws.String("group-name"), Values: []*string{aws.String("default")}},
	}})
	require.NoError(t, err, "describe-security-groups default")
	require.NotEmpty(t, out.SecurityGroups, "no default SG for %s", vpcID)
	return aws.StringValue(out.SecurityGroups[0].GroupId)
}

// pingOnce reports whether one burst from tgt to dst got every reply.
func pingOnce(t *testing.T, tgt harness.SSHTarget, dst string) bool {
	t.Helper()
	out, err := runSSHQuiet(tgt, fmt.Sprintf("ping -c 3 -W 2 %s >/dev/null 2>&1 && echo yes || echo no", dst))
	require.NoErrorf(t, err, "ssh to %s: %s", tgt.Host, out)
	return strings.TrimSpace(out) == "yes"
}

// requirePingBlocked asserts dst stays unreachable from tgt across several
// bursts, after any earlier path has had time to be withdrawn.
func requirePingBlocked(t *testing.T, tgt harness.SSHTarget, dst, why string) {
	t.Helper()
	harness.Eventually(t, func() bool { return !pingOnce(t, tgt, dst) }, 60*time.Second, 3*time.Second, "%s: still reachable", why)
	for range 3 {
		require.Falsef(t, pingOnce(t, tgt, dst), "%s: %s reached %s", why, tgt.Host, dst)
	}
}

func requirePingConverged(t *testing.T, tgt harness.SSHTarget, dst, why string) {
	t.Helper()
	out, ok := pingConverged(tgt, dst, 90*time.Second)
	require.Truef(t, ok, "%s: %s never reached %s\n%s", why, tgt.Host, dst, out)
}

func createPeering(t *testing.T, c *harness.AWSClient, vpc, peer string) string {
	t.Helper()
	out, err := c.EC2.CreateVpcPeeringConnection(&ec2.CreateVpcPeeringConnectionInput{VpcId: aws.String(vpc), PeerVpcId: aws.String(peer)})
	require.NoError(t, err, "create-vpc-peering-connection %s -> %s", vpc, peer)
	id := aws.StringValue(out.VpcPeeringConnection.VpcPeeringConnectionId)
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteVpcPeeringConnection(&ec2.DeleteVpcPeeringConnectionInput{VpcPeeringConnectionId: aws.String(id)})
	})
	harness.Detail(t, "pcx", id, "requester", vpc, "accepter", peer)
	return id
}

func acceptPeering(t *testing.T, c *harness.AWSClient, id string) {
	t.Helper()
	_, err := c.EC2.AcceptVpcPeeringConnection(&ec2.AcceptVpcPeeringConnectionInput{VpcPeeringConnectionId: aws.String(id)})
	require.NoError(t, err, "accept-vpc-peering-connection %s", id)
	harness.Eventually(t, func() bool { return peeringStatus(t, c, id) == "active" }, 90*time.Second, 2*time.Second,
		"%s never became active", id)
}

func peeringStatus(t *testing.T, c *harness.AWSClient, id string) string {
	t.Helper()
	out, err := c.EC2.DescribeVpcPeeringConnections(&ec2.DescribeVpcPeeringConnectionsInput{VpcPeeringConnectionIds: []*string{aws.String(id)}})
	require.NoError(t, err, "describe-vpc-peering-connections %s", id)
	if len(out.VpcPeeringConnections) == 0 || out.VpcPeeringConnections[0].Status == nil {
		return ""
	}
	return aws.StringValue(out.VpcPeeringConnections[0].Status.Code)
}

func peeringRoute(t *testing.T, c *harness.AWSClient, rtbID, dst, pcxID string) {
	t.Helper()
	_, err := c.EC2.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId: aws.String(rtbID), DestinationCidrBlock: aws.String(dst), VpcPeeringConnectionId: aws.String(pcxID),
	})
	require.NoError(t, err, "create-route %s %s -> %s", rtbID, dst, pcxID)
}

func routeState(t *testing.T, c *harness.AWSClient, rtbID, dst string) string {
	t.Helper()
	out, err := c.EC2.DescribeRouteTables(&ec2.DescribeRouteTablesInput{RouteTableIds: []*string{aws.String(rtbID)}})
	require.NoError(t, err, "describe-route-tables %s", rtbID)
	require.Len(t, out.RouteTables, 1)
	for _, r := range out.RouteTables[0].Routes {
		if aws.StringValue(r.DestinationCidrBlock) == dst {
			return aws.StringValue(r.State)
		}
	}
	return ""
}

func requireAWSCode(t *testing.T, err error, code string) {
	t.Helper()
	require.Error(t, err)
	var aerr awserr.Error
	require.ErrorAs(t, err, &aerr)
	require.Equal(t, code, aerr.Code(), aerr.Message())
}

// runVPCPeering proves same-account peering end to end: no path until both
// sides route, no transit, no edge-to-edge egress, a guard on the link, and
// DeleteVpc's dependency rules.
func runVPCPeering(t *testing.T, fix *Fixture) {
	if !fix.PublicPool {
		t.Skip("VPC peering scenario reaches its guests over public addresses")
	}
	harness.Phase(t, "Single — VPC peering: reachability, transit, edge-to-edge and DeleteVpc")
	c := fix.AWS
	ami := needAMI(t, fix)
	instType, _ := harness.DiscoverNanoInstanceType(t, fix.Harness)
	keyName, keyPath := harness.EnsureKeyPair(t, fix.Harness)

	a := newPeeringGuest(t, fix, "10.150.0.0/16", "10.150.1.0/24", ami, instType, keyName, keyPath)
	b := newPeeringGuest(t, fix, "10.151.0.0/16", "10.151.1.0/24", ami, instType, keyName, keyPath)

	harness.Step(t, "1. no path before peering")
	requirePingBlocked(t, a.tgt, b.privateIP, "before peering")

	pcx := createPeering(t, c, a.vpcID, b.vpcID)
	acceptPeering(t, c, pcx)

	harness.Step(t, "2. active with no routes")
	requirePingBlocked(t, a.tgt, b.privateIP, "active with no routes")

	harness.Step(t, "3. a route on one side only drops the reply")
	peeringRoute(t, c, a.rtbID, b.cidr, pcx)
	requirePingBlocked(t, a.tgt, b.privateIP, "route on A only")

	harness.Step(t, "4. routes on both sides")
	peeringRoute(t, c, b.rtbID, a.cidr, pcx)
	requirePingConverged(t, a.tgt, b.privateIP, "A -> B over the peering")
	requirePingConverged(t, b.tgt, a.privateIP, "B -> A over the peering")
	require.Equal(t, "active", routeState(t, c, a.rtbID, b.cidr))

	t.Run("NoTransit", func(t *testing.T) {
		harness.Step(t, "5. C peered to B cannot reach A through B")
		cg := newPeeringGuest(t, fix, "10.152.0.0/16", "10.152.1.0/24", ami, instType, keyName, keyPath)
		bc := createPeering(t, c, cg.vpcID, b.vpcID)
		acceptPeering(t, c, bc)
		peeringRoute(t, c, cg.rtbID, b.cidr, bc)
		peeringRoute(t, c, b.rtbID, cg.cidr, bc)
		peeringRoute(t, c, cg.rtbID, a.cidr, bc)
		requirePingConverged(t, cg.tgt, b.privateIP, "C -> B control")
		requirePingBlocked(t, cg.tgt, a.privateIP, "C -> A through B")
	})

	t.Run("NoEdgeToEdge", func(t *testing.T) {
		harness.Step(t, "6. B cannot egress through A's internet gateway")
		const probe = "8.8.8.8"
		if !pingOnce(t, b.tgt, probe) || !pingOnce(t, a.tgt, probe) {
			t.Skip("WAN egress unavailable from the guests; edge-to-edge needs a working baseline")
		}
		_, err := c.EC2.CreateRoute(&ec2.CreateRouteInput{
			RouteTableId: aws.String(b.rtbID), DestinationCidrBlock: aws.String(probe + "/32"), VpcPeeringConnectionId: aws.String(pcx),
		})
		require.NoError(t, err, "create-route probe -> pcx")
		t.Cleanup(func() {
			_, _ = c.EC2.DeleteRoute(&ec2.DeleteRouteInput{RouteTableId: aws.String(b.rtbID), DestinationCidrBlock: aws.String(probe + "/32")})
		})
		requirePingBlocked(t, b.tgt, probe, "B to the internet over the peering")
		require.True(t, pingOnce(t, a.tgt, probe), "A's own egress must be unaffected")
	})

	t.Run("GuardDropsSpoofedSource", func(t *testing.T) {
		harness.SkipIfNoOVN(t)
		harness.Step(t, "guard: a packet over the link from outside B's range is dropped at A's router")
		inport := "pcx-" + pcx + "-" + a.vpcID
		peerPort := "pcx-" + pcx + "-" + b.vpcID
		mac := func(port string) string {
			return strings.Trim(harness.OvnNbctl(t, "get", "Logical_Router_Port", port, "mac"), `"`)
		}
		flow := fmt.Sprintf(`inport == %q && eth.src == %s && eth.dst == %s && ip4.src == 10.152.1.10 && ip4.dst == %s && ip.ttl == 64 && icmp4`,
			inport, mac(peerPort), mac(inport), a.privateIP)
		trace := harness.OvnTrace(t, "vpc-"+a.vpcID, flow)
		require.Containsf(t, trace, "priority 1400", "spoofed packet did not hit the peering guard\n%s", trace)
		require.Containsf(t, trace, "drop;", "spoofed packet was not dropped\n%s", trace)
	})

	t.Run("DeleteVpcWhileActive", func(t *testing.T) {
		harness.Step(t, "8. DeleteVpc refuses an active connection")
		x := harness.CreateVPC(t, c, "10.153.0.0/16")
		t.Cleanup(func() { harness.DeleteVPC(t, c, x) })
		y := harness.CreateVPC(t, c, "10.154.0.0/16")
		t.Cleanup(func() { harness.DeleteVPC(t, c, y) })
		xy := createPeering(t, c, x, y)
		acceptPeering(t, c, xy)
		_, err := c.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(x)})
		requireAWSCode(t, err, "DependencyViolation")
		require.Equal(t, "active", peeringStatus(t, c, xy))

		_, err = c.EC2.DeleteVpcPeeringConnection(&ec2.DeleteVpcPeeringConnectionInput{VpcPeeringConnectionId: aws.String(xy)})
		require.NoError(t, err)
		_, err = c.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(x)})
		require.NoError(t, err, "DeleteVpc once the connection is deleted")

		harness.Step(t, "8. a pending connection does not block DeleteVpc and reads rejected")
		z := harness.CreateVPC(t, c, "10.155.0.0/16")
		t.Cleanup(func() { harness.DeleteVPC(t, c, z) })
		yz := createPeering(t, c, y, z)
		_, err = c.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(z)})
		require.NoError(t, err, "DeleteVpc with a pending connection")
		require.Equal(t, "rejected", peeringStatus(t, c, yz))
	})

	harness.Step(t, "7. after delete, no path and both routes blackhole")
	_, err := c.EC2.DeleteVpcPeeringConnection(&ec2.DeleteVpcPeeringConnectionInput{VpcPeeringConnectionId: aws.String(pcx)})
	require.NoError(t, err, "delete-vpc-peering-connection")
	requirePingBlocked(t, a.tgt, b.privateIP, "after delete")
	require.Equal(t, "blackhole", routeState(t, c, a.rtbID, b.cidr))
	require.Equal(t, "blackhole", routeState(t, c, b.rtbID, a.cidr))
	for _, rtb := range []struct{ id, dst string }{{a.rtbID, b.cidr}, {b.rtbID, a.cidr}} {
		_, err := c.EC2.DeleteRoute(&ec2.DeleteRouteInput{RouteTableId: aws.String(rtb.id), DestinationCidrBlock: aws.String(rtb.dst)})
		require.NoError(t, err, "delete blackhole route")
	}
}
