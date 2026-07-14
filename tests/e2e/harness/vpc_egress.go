//go:build e2e

package harness

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/require"
)

// CreateVPC creates a VPC with the given CIDR and returns its ID.
func CreateVPC(t *testing.T, c *AWSClient, cidr string) string {
	t.Helper()
	out, err := c.EC2.CreateVpc(&ec2.CreateVpcInput{CidrBlock: aws.String(cidr)}) // e2e:allow-create
	require.NoError(t, err, "create-vpc")
	vpcID := aws.StringValue(out.Vpc.VpcId)
	t.Logf("VPC: %s", vpcID)
	return vpcID
}

// DeleteVPC deletes vpcID, logging (not failing) on error so teardown proceeds.
func DeleteVPC(t *testing.T, c *AWSClient, vpcID string) {
	t.Helper()
	if vpcID == "" {
		return
	}
	if _, err := c.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}); err != nil {
		t.Logf("delete VPC %s: %v", vpcID, err)
	}
}

// CreateSubnet creates a subnet in vpcID with the given CIDR and returns its ID.
func CreateSubnet(t *testing.T, c *AWSClient, vpcID, cidr string) string {
	t.Helper()
	out, err := c.EC2.CreateSubnet(&ec2.CreateSubnetInput{ // e2e:allow-create
		VpcId:     aws.String(vpcID),
		CidrBlock: aws.String(cidr),
	})
	require.NoError(t, err, "create-subnet")
	subnetID := aws.StringValue(out.Subnet.SubnetId)
	t.Logf("subnet: %s", subnetID)
	return subnetID
}

// DeleteSubnet deletes subnetID, logging (not failing) on error so teardown proceeds.
func DeleteSubnet(t *testing.T, c *AWSClient, subnetID string) {
	t.Helper()
	if subnetID == "" {
		return
	}
	if _, err := c.EC2.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String(subnetID)}); err != nil {
		t.Logf("delete subnet %s: %v", subnetID, err)
	}
}

// WorkerEgress bundles the IGW/public-subnet/NAT-gateway resources that give a
// private worker subnet internet egress, plus the IDs needed to tear it down.
type WorkerEgress struct {
	VPCID           string
	WorkerSubnetID  string
	IGWID           string
	PubSubnetID     string
	PubRTID         string
	PubRTAssocID    string
	EIPAllocID      string
	NATGWID         string
	WorkerRTID      string
	WorkerRTAssocID string
}

// CreateWorkerEgress gives vpcID the egress a real customer VPC needs for
// private workers: an IGW-fronted public subnet (pubCIDR) hosting a NAT
// Gateway, with workerSubnetID default-routed to that NAT Gateway. AttachIGW
// deliberately programs no SNAT (mirrors AWS: an IGW serves only public-IP
// instances), so a private worker reaches a public NLB endpoint and pulls
// public images only through the NAT Gateway's subnet-wide SNAT.
func CreateWorkerEgress(t *testing.T, c *AWSClient, vpcID, workerSubnetID, pubCIDR string) *WorkerEgress {
	t.Helper()
	eg := &WorkerEgress{VPCID: vpcID, WorkerSubnetID: workerSubnetID}

	igwOut, err := c.EC2.CreateInternetGateway(&ec2.CreateInternetGatewayInput{}) // e2e:allow-create
	require.NoError(t, err, "create-internet-gateway")
	eg.IGWID = aws.StringValue(igwOut.InternetGateway.InternetGatewayId)
	_, err = c.EC2.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(eg.IGWID),
		VpcId:             aws.String(vpcID),
	})
	require.NoError(t, err, "attach-internet-gateway")

	// Public subnet routed straight to the IGW; the NAT Gateway lives here.
	pubOut, err := c.EC2.CreateSubnet(&ec2.CreateSubnetInput{ // e2e:allow-create
		VpcId:     aws.String(vpcID),
		CidrBlock: aws.String(pubCIDR),
	})
	require.NoError(t, err, "create-public-subnet")
	eg.PubSubnetID = aws.StringValue(pubOut.Subnet.SubnetId)

	pubRT, err := c.EC2.CreateRouteTable(&ec2.CreateRouteTableInput{VpcId: aws.String(vpcID)}) // e2e:allow-create
	require.NoError(t, err, "create-public-route-table")
	eg.PubRTID = aws.StringValue(pubRT.RouteTable.RouteTableId)
	_, err = c.EC2.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(eg.PubRTID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String(eg.IGWID),
	})
	require.NoError(t, err, "create-public-route")
	pubAssoc, err := c.EC2.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(eg.PubRTID),
		SubnetId:     aws.String(eg.PubSubnetID),
	})
	require.NoError(t, err, "associate-public-route-table")
	eg.PubRTAssocID = aws.StringValue(pubAssoc.AssociationId)

	// Elastic IP + NAT Gateway in the public subnet.
	eip, err := c.EC2.AllocateAddress(&ec2.AllocateAddressInput{Domain: aws.String("vpc")}) // e2e:allow-create
	require.NoError(t, err, "allocate-address")
	eg.EIPAllocID = aws.StringValue(eip.AllocationId)
	natOut, err := c.EC2.CreateNatGateway(&ec2.CreateNatGatewayInput{ // e2e:allow-create
		SubnetId:     aws.String(eg.PubSubnetID),
		AllocationId: aws.String(eg.EIPAllocID),
	})
	require.NoError(t, err, "create-nat-gateway")
	eg.NATGWID = aws.StringValue(natOut.NatGateway.NatGatewayId)
	waitNatGatewayAvailable(t, c, eg.NATGWID)

	// Worker (private) subnet default-routes to the NAT Gateway; associating
	// the route table triggers the subnet-wide SNAT egress reroute.
	workerRT, err := c.EC2.CreateRouteTable(&ec2.CreateRouteTableInput{VpcId: aws.String(vpcID)}) // e2e:allow-create
	require.NoError(t, err, "create-worker-route-table")
	eg.WorkerRTID = aws.StringValue(workerRT.RouteTable.RouteTableId)
	_, err = c.EC2.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(eg.WorkerRTID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		NatGatewayId:         aws.String(eg.NATGWID),
	})
	require.NoError(t, err, "create-worker-route")
	workerAssoc, err := c.EC2.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(eg.WorkerRTID),
		SubnetId:     aws.String(workerSubnetID),
	})
	require.NoError(t, err, "associate-worker-route-table")
	eg.WorkerRTAssocID = aws.StringValue(workerAssoc.AssociationId)
	t.Logf("egress: igw=%s nat=%s eip=%s pubrt=%s workerrt=%s",
		eg.IGWID, eg.NATGWID, eg.EIPAllocID, eg.PubRTID, eg.WorkerRTID)
	return eg
}

// waitNatGatewayAvailable blocks until the NAT Gateway reports "available";
// SNAT is not programmed on the VPC router until then.
func waitNatGatewayAvailable(t *testing.T, c *AWSClient, natID string) {
	t.Helper()
	const timeout = 3 * time.Minute
	deadline := time.Now().Add(timeout)
	for {
		out, err := c.EC2.DescribeNatGateways(&ec2.DescribeNatGatewaysInput{
			NatGatewayIds: aws.StringSlice([]string{natID}),
		})
		if err == nil && len(out.NatGateways) > 0 {
			switch aws.StringValue(out.NatGateways[0].State) {
			case "available":
				return
			case "failed", "deleted":
				t.Fatalf("nat gateway %s entered terminal state %q", natID, aws.StringValue(out.NatGateways[0].State))
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("nat gateway %s not available within %s", natID, timeout)
		}
		time.Sleep(3 * time.Second)
	}
}

// DeleteWorkerEgress tears the egress topology down in reverse: the worker
// route table releases the private subnet, then the NAT Gateway is deleted and
// drained before the public subnet + EIP it pins can be removed.
func DeleteWorkerEgress(t *testing.T, c *AWSClient, eg *WorkerEgress) {
	t.Helper()
	if eg == nil {
		return
	}
	if eg.WorkerRTAssocID != "" {
		if _, err := c.EC2.DisassociateRouteTable(&ec2.DisassociateRouteTableInput{
			AssociationId: aws.String(eg.WorkerRTAssocID),
		}); err != nil {
			t.Logf("disassociate worker route table %s: %v", eg.WorkerRTAssocID, err)
		}
	}
	if eg.WorkerRTID != "" {
		if _, err := c.EC2.DeleteRouteTable(&ec2.DeleteRouteTableInput{
			RouteTableId: aws.String(eg.WorkerRTID),
		}); err != nil {
			t.Logf("delete worker route table %s: %v", eg.WorkerRTID, err)
		}
	}
	if eg.NATGWID != "" {
		if _, err := c.EC2.DeleteNatGateway(&ec2.DeleteNatGatewayInput{
			NatGatewayId: aws.String(eg.NATGWID),
		}); err != nil {
			t.Logf("delete nat gateway %s: %v", eg.NATGWID, err)
		}
		waitNatGatewayDeleted(t, c, eg.NATGWID)
	}
	if eg.EIPAllocID != "" {
		if _, err := c.EC2.ReleaseAddress(&ec2.ReleaseAddressInput{
			AllocationId: aws.String(eg.EIPAllocID),
		}); err != nil {
			t.Logf("release address %s: %v", eg.EIPAllocID, err)
		}
	}
	if eg.PubRTAssocID != "" {
		if _, err := c.EC2.DisassociateRouteTable(&ec2.DisassociateRouteTableInput{
			AssociationId: aws.String(eg.PubRTAssocID),
		}); err != nil {
			t.Logf("disassociate public route table %s: %v", eg.PubRTAssocID, err)
		}
	}
	if eg.PubRTID != "" {
		if _, err := c.EC2.DeleteRouteTable(&ec2.DeleteRouteTableInput{
			RouteTableId: aws.String(eg.PubRTID),
		}); err != nil {
			t.Logf("delete public route table %s: %v", eg.PubRTID, err)
		}
	}
	if eg.PubSubnetID != "" {
		if _, err := c.EC2.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String(eg.PubSubnetID)}); err != nil {
			t.Logf("delete public subnet %s: %v", eg.PubSubnetID, err)
		}
	}
	if eg.IGWID != "" {
		if _, err := c.EC2.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
			InternetGatewayId: aws.String(eg.IGWID),
			VpcId:             aws.String(eg.VPCID),
		}); err != nil {
			t.Logf("detach igw %s: %v", eg.IGWID, err)
		}
		if _, err := c.EC2.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{
			InternetGatewayId: aws.String(eg.IGWID),
		}); err != nil {
			t.Logf("delete igw %s: %v", eg.IGWID, err)
		}
	}
}

// waitNatGatewayDeleted blocks until the NAT Gateway drains to "deleted" so the
// public subnet + EIP it holds can be released; best-effort on timeout.
func waitNatGatewayDeleted(t *testing.T, c *AWSClient, natID string) {
	t.Helper()
	const timeout = 3 * time.Minute
	deadline := time.Now().Add(timeout)
	for {
		out, err := c.EC2.DescribeNatGateways(&ec2.DescribeNatGatewaysInput{
			NatGatewayIds: aws.StringSlice([]string{natID}),
		})
		if err != nil || len(out.NatGateways) == 0 {
			return
		}
		if aws.StringValue(out.NatGateways[0].State) == "deleted" {
			return
		}
		if time.Now().After(deadline) {
			t.Logf("nat gateway %s not deleted within %s; continuing teardown", natID, timeout)
			return
		}
		time.Sleep(3 * time.Second)
	}
}
