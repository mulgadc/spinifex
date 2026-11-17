//go:build e2e

package multinode

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

const peeringCIDR = "10.210.0.0/16"

// runMultinodeVPCPeering peers the default VPC with a fresh one and pings
// between guests on different nodes, so the link carries traffic across
// chassis rather than within one.
func runMultinodeVPCPeering(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode — VPC peering across hosts")
	c := fix.AWS
	defVPC, defSG, defSubnet := harness.DiscoverDefaultVPC(t, c)
	instType, arch := harness.DiscoverNanoInstanceType(t, fix.Harness)
	amiID := needAMI(t, fix, arch)
	keyName, keyPath := harness.EnsureKeyPair(t, fix.Harness)
	defCIDR := vpcCIDR(t, c, defVPC)

	// A spread group puts the two ends on different hosts.
	const pgName = "peering-spread"
	_, err := c.EC2.CreatePlacementGroup(&ec2.CreatePlacementGroupInput{GroupName: aws.String(pgName), Strategy: aws.String("spread")})
	require.NoError(t, err, "create-placement-group %s", pgName)
	t.Cleanup(func() { _, _ = c.EC2.DeletePlacementGroup(&ec2.DeletePlacementGroupInput{GroupName: aws.String(pgName)}) })
	spread := func(in *ec2.RunInstancesInput) { in.Placement = &ec2.Placement{GroupName: aws.String(pgName)} }

	runnerSG := harness.EnsureSG(t, fix.Harness, defVPC, "peering-runnersg")
	harness.AuthorizeSSHIngress(t, c, runnerSG)
	srcID := baselineLaunch(t, fix, amiID, instType, keyName, defSubnet, []string{defSG, runnerSG}, spread)
	srcNode := harness.InstanceHostingNode(t, fix.Cluster, srcID)
	require.NotNil(t, srcNode, "source %s has no hosting node", srcID)

	peerVPC := harness.CreateVPC(t, c, peeringCIDR)
	t.Cleanup(func() { harness.DeleteVPC(t, c, peerVPC) })
	peerSubnet := harness.CreateSubnet(t, c, peerVPC, "10.210.1.0/24")
	t.Cleanup(func() { harness.DeleteSubnet(t, c, peerSubnet) })
	peerSG := harness.EnsureSG(t, fix.Harness, peerVPC, "peering-icmp")
	harness.AuthorizeICMPIngress(t, c, peerSG)

	dstID := baselineLaunch(t, fix, amiID, instType, keyName, peerSubnet, []string{peerSG}, spread)
	dstNode := harness.InstanceHostingNode(t, fix.Cluster, dstID)
	require.NotNil(t, dstNode, "peer %s has no hosting node", dstID)
	require.NotEqual(t, srcNode.Name, dstNode.Name, "spread placement colocated %s and %s", srcID, dstID)
	dstPriv := instancePrivateIP(t, fix, dstID)

	out, err := c.EC2.CreateVpcPeeringConnection(&ec2.CreateVpcPeeringConnectionInput{VpcId: aws.String(defVPC), PeerVpcId: aws.String(peerVPC)})
	require.NoError(t, err, "create-vpc-peering-connection")
	pcx := aws.StringValue(out.VpcPeeringConnection.VpcPeeringConnectionId)
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteVpcPeeringConnection(&ec2.DeleteVpcPeeringConnectionInput{VpcPeeringConnectionId: aws.String(pcx)})
	})
	_, err = c.EC2.AcceptVpcPeeringConnection(&ec2.AcceptVpcPeeringConnectionInput{VpcPeeringConnectionId: aws.String(pcx)})
	require.NoError(t, err, "accept-vpc-peering-connection")

	for _, r := range []struct{ vpc, dst string }{{defVPC, peeringCIDR}, {peerVPC, defCIDR}} {
		rtb := mainRouteTableID(t, c, r.vpc)
		_, err := c.EC2.CreateRoute(&ec2.CreateRouteInput{
			RouteTableId: aws.String(rtb), DestinationCidrBlock: aws.String(r.dst), VpcPeeringConnectionId: aws.String(pcx),
		})
		require.NoError(t, err, "create-route %s -> %s", r.dst, pcx)
		t.Cleanup(func() {
			_, _ = c.EC2.DeleteRoute(&ec2.DeleteRouteInput{RouteTableId: aws.String(rtb), DestinationCidrBlock: aws.String(r.dst)})
		})
	}

	host, port := harness.GuestSSHEndpoint(t, c, fix.Cluster, srcID)
	harness.GuestSSHReady(t, host, port, "ubuntu", keyPath, harness.WithTimeout(3*time.Minute), harness.WithPoll(3*time.Second))
	harness.Step(t, "ping %s (%s) from %s over %s across hosts", dstID, dstPriv, srcID, pcx)
	got, ok, attempts, elapsed := pingConvergedCrossHost(keyPath, host, port, dstPriv, 2*time.Minute, nil)
	harness.Detail(t, "ping_attempts", attempts, "ping_converged", ok, "ping_convergence_ms", elapsed.Milliseconds())
	require.Truef(t, ok, "cross-host peered ping never converged\n%s", got)
}

func vpcCIDR(t *testing.T, c *harness.AWSClient, vpcID string) string {
	t.Helper()
	out, err := c.EC2.DescribeVpcs(&ec2.DescribeVpcsInput{VpcIds: []*string{aws.String(vpcID)}})
	require.NoError(t, err, "describe-vpcs %s", vpcID)
	require.Len(t, out.Vpcs, 1)
	return aws.StringValue(out.Vpcs[0].CidrBlock)
}

func mainRouteTableID(t *testing.T, c *harness.AWSClient, vpcID string) string {
	t.Helper()
	out, err := c.EC2.DescribeRouteTables(&ec2.DescribeRouteTablesInput{Filters: []*ec2.Filter{
		{Name: aws.String("vpc-id"), Values: []*string{aws.String(vpcID)}},
		{Name: aws.String("association.main"), Values: []*string{aws.String("true")}},
	}})
	require.NoError(t, err, "describe-route-tables main %s", vpcID)
	require.NotEmpty(t, out.RouteTables, "no main route table for %s", vpcID)
	return aws.StringValue(out.RouteTables[0].RouteTableId)
}
