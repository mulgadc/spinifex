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

// tgwActivationBudget bounds the wait for the cluster to reach the Transit
// Gateway feature level after it forms.
const tgwActivationBudget = 5 * time.Minute

// createTransitGateway creates a gateway once the feature level offers it and
// waits for it to be available.
func createTransitGateway(t *testing.T, c *harness.AWSClient) string {
	t.Helper()
	var id string
	harness.EventuallyErr(t, func() error {
		out, err := c.EC2.CreateTransitGateway(&ec2.CreateTransitGatewayInput{})
		if err != nil {
			return err
		}
		id = aws.StringValue(out.TransitGateway.TransitGatewayId)
		return nil
	}, tgwActivationBudget, 5*time.Second)
	t.Cleanup(func() { deleteTransitGateway(t, c, id) })
	harness.Eventually(t, func() bool { return tgwState(t, c, id) == "available" }, 2*time.Minute, 2*time.Second,
		"%s never became available", id)
	harness.Detail(t, "tgw", id)
	return id
}

func tgwState(t *testing.T, c *harness.AWSClient, id string) string {
	t.Helper()
	out, err := c.EC2.DescribeTransitGateways(&ec2.DescribeTransitGatewaysInput{TransitGatewayIds: []*string{aws.String(id)}})
	require.NoError(t, err, "describe-transit-gateways %s", id)
	if len(out.TransitGateways) == 0 {
		return ""
	}
	return aws.StringValue(out.TransitGateways[0].State)
}

func deleteTransitGateway(t *testing.T, c *harness.AWSClient, id string) {
	t.Helper()
	if _, err := c.EC2.DeleteTransitGateway(&ec2.DeleteTransitGatewayInput{TransitGatewayId: aws.String(id)}); err != nil {
		t.Logf("delete transit gateway %s: %v", id, err)
		return
	}
	harness.Eventually(t, func() bool { return tgwState(t, c, id) == "deleted" }, 2*time.Minute, 2*time.Second,
		"%s never reached deleted", id)
}

// attachVPC attaches g's VPC through its subnet and waits for the attachment
// to be available. Its cleanup waits for deleted, so the VPC delete after it
// finds no dependency.
func attachVPC(t *testing.T, c *harness.AWSClient, tgwID string, g *peeringGuest) string {
	t.Helper()
	out, err := c.EC2.CreateTransitGatewayVpcAttachment(&ec2.CreateTransitGatewayVpcAttachmentInput{
		TransitGatewayId: aws.String(tgwID), VpcId: aws.String(g.vpcID), SubnetIds: []*string{aws.String(g.subnetID)},
	})
	require.NoError(t, err, "create-transit-gateway-vpc-attachment %s", g.vpcID)
	id := aws.StringValue(out.TransitGatewayVpcAttachment.TransitGatewayAttachmentId)
	require.Equal(t, "pending", aws.StringValue(out.TransitGatewayVpcAttachment.State))
	t.Cleanup(func() { detachVPC(t, c, id) })
	harness.Eventually(t, func() bool { return attachmentState(t, c, id) == "available" }, 2*time.Minute, 2*time.Second,
		"%s never became available", id)
	harness.Detail(t, "attachment", id, "vpc", g.vpcID)
	return id
}

func attachmentState(t *testing.T, c *harness.AWSClient, id string) string {
	t.Helper()
	out, err := c.EC2.DescribeTransitGatewayVpcAttachments(&ec2.DescribeTransitGatewayVpcAttachmentsInput{
		TransitGatewayAttachmentIds: []*string{aws.String(id)},
	})
	require.NoError(t, err, "describe-transit-gateway-vpc-attachments %s", id)
	if len(out.TransitGatewayVpcAttachments) == 0 {
		return ""
	}
	return aws.StringValue(out.TransitGatewayVpcAttachments[0].State)
}

func detachVPC(t *testing.T, c *harness.AWSClient, id string) {
	t.Helper()
	_, err := c.EC2.DeleteTransitGatewayVpcAttachment(&ec2.DeleteTransitGatewayVpcAttachmentInput{TransitGatewayAttachmentId: aws.String(id)})
	if err != nil {
		t.Logf("delete attachment %s: %v", id, err)
		return
	}
	harness.Eventually(t, func() bool { return attachmentState(t, c, id) == "deleted" }, 2*time.Minute, 2*time.Second,
		"%s never reached deleted", id)
}

func tgwRoute(t *testing.T, c *harness.AWSClient, rtbID, dst, tgwID string) {
	t.Helper()
	_, err := c.EC2.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId: aws.String(rtbID), DestinationCidrBlock: aws.String(dst), TransitGatewayId: aws.String(tgwID),
	})
	require.NoError(t, err, "create-route %s %s -> %s", rtbID, dst, tgwID)
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteRoute(&ec2.DeleteRouteInput{RouteTableId: aws.String(rtbID), DestinationCidrBlock: aws.String(dst)})
	})
}

func requireAWSMessage(t *testing.T, err error, code, contains string) {
	t.Helper()
	requireAWSCode(t, err, code)
	var aerr awserr.Error
	require.ErrorAs(t, err, &aerr)
	require.Contains(t, aerr.Message(), contains)
}

// runTransitGateway proves two VPCs routed through one Transit Gateway: no
// path until both tables route, the AWS refusals while attached, the guard on
// the link, and a blackhole after the attachment is deleted.
func runTransitGateway(t *testing.T, fix *Fixture) {
	if !fix.PublicPool {
		t.Skip("Transit Gateway scenario reaches its guests over public addresses")
	}
	harness.Phase(t, "Single — Transit Gateway: reachability, refusals, guard and blackhole")
	c := fix.AWS
	ami := needAMI(t, fix)
	instType, _ := harness.DiscoverNanoInstanceType(t, fix.Harness)
	keyName, keyPath := harness.EnsureKeyPair(t, fix.Harness)

	a := newPeeringGuest(t, fix, "10.160.0.0/16", "10.160.1.0/24", ami, instType, keyName, keyPath)
	b := newPeeringGuest(t, fix, "10.161.0.0/16", "10.161.1.0/24", ami, instType, keyName, keyPath)

	harness.Step(t, "1. gateway and attachments reach available")
	tgw := createTransitGateway(t, c)
	attA := attachVPC(t, c, tgw, a)
	attB := attachVPC(t, c, tgw, b)

	harness.Step(t, "2. attached with no routes")
	requirePingBlocked(t, a.tgt, b.privateIP, "attached with no routes")

	harness.Step(t, "3. a route on one side only drops the reply")
	tgwRoute(t, c, a.rtbID, b.cidr, tgw)
	requirePingBlocked(t, a.tgt, b.privateIP, "route on A only")

	harness.Step(t, "4. routes on both sides")
	tgwRoute(t, c, b.rtbID, a.cidr, tgw)
	requirePingConverged(t, a.tgt, b.privateIP, "A -> B over the Transit Gateway")
	requirePingConverged(t, b.tgt, a.privateIP, "B -> A over the Transit Gateway")
	require.Equal(t, "active", routeState(t, c, a.rtbID, b.cidr))

	t.Run("RefusalsWhileAttached", func(t *testing.T) {
		harness.Step(t, "5. AWS refusals while attached")
		_, err := c.EC2.DeleteTransitGateway(&ec2.DeleteTransitGatewayInput{TransitGatewayId: aws.String(tgw)})
		requireAWSMessage(t, err, "IncorrectState", fmt.Sprintf("%s has non-deleted VPC Attachments: ", tgw))
		require.Equal(t, "available", tgwState(t, c, tgw))

		_, err = c.EC2.CreateTransitGatewayVpcAttachment(&ec2.CreateTransitGatewayVpcAttachmentInput{
			TransitGatewayId: aws.String(tgw), VpcId: aws.String(a.vpcID), SubnetIds: []*string{aws.String(a.subnetID)},
		})
		requireAWSMessage(t, err, "DuplicateTransitGatewayAttachment", "with same VPC ID")

		// A VPC with no instance, so only the attachment blocks its deletes.
		e := &peeringGuest{vpcID: harness.CreateVPC(t, c, "10.162.0.0/16")}
		t.Cleanup(func() { harness.DeleteVPC(t, c, e.vpcID) })
		e.subnetID = harness.CreateSubnet(t, c, e.vpcID, "10.162.1.0/24")
		t.Cleanup(func() { harness.DeleteSubnet(t, c, e.subnetID) })
		spare := harness.CreateSubnet(t, c, e.vpcID, "10.162.2.0/24")
		attE := attachVPC(t, c, tgw, e)
		_, err = c.EC2.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String(e.subnetID)})
		requireAWSMessage(t, err, "DependencyViolation", fmt.Sprintf("The subnet '%s' has dependencies", e.subnetID))
		_, err = c.EC2.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String(spare)})
		require.NoError(t, err, "a subnet the attachment does not use deletes")
		_, err = c.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(e.vpcID)})
		requireAWSMessage(t, err, "DependencyViolation", fmt.Sprintf("The vpc '%s' has dependencies", e.vpcID))
		detachVPC(t, c, attE)
		_, err = c.EC2.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String(e.subnetID)})
		require.NoError(t, err, "DeleteSubnet once the attachment is deleted")
		_, err = c.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(e.vpcID)})
		require.NoError(t, err, "DeleteVpc once the attachment is deleted")
		e.subnetID, e.vpcID = "", ""

		_, err = c.EC2.CreateRoute(&ec2.CreateRouteInput{
			RouteTableId: aws.String(a.rtbID), DestinationCidrBlock: aws.String("10.160.1.0/24"), TransitGatewayId: aws.String(tgw),
		})
		requireAWSMessage(t, err, "InvalidParameterValue", "equal to or more specific than")
	})

	t.Run("GuardDropsOutsideDestination", func(t *testing.T) {
		harness.SkipIfNoOVN(t)
		harness.Step(t, "guard: a packet from the gateway for outside A's range is dropped at A's router")
		requirePolicyDrop(t, tracePolicy(t, a.vpcID, "tgwv-"+attA, b.privateIP, "8.8.8.8"), 1400, "gateway -> outside A's range")
	})

	harness.Step(t, "6. after B's attachment is deleted, no path, and A's route stays active")
	// A route's state follows its own VPC's attachment, as on AWS.
	detachVPC(t, c, attB)
	requirePingBlocked(t, a.tgt, b.privateIP, "after B's attachment is deleted")
	require.Equal(t, "active", routeState(t, c, a.rtbID, b.cidr))

	harness.Step(t, "7. after A's attachment is deleted, A's route is a blackhole")
	detachVPC(t, c, attA)
	harness.Eventually(t, func() bool { return routeState(t, c, a.rtbID, b.cidr) == "blackhole" }, 60*time.Second, 2*time.Second,
		"A's route to B never became a blackhole")
	t.Run("BlackholeBeatsDefaultRoute", func(t *testing.T) {
		harness.SkipIfNoOVN(t)
		stage := awaitPolicy(t, a.vpcID, "rtr-"+a.subnetID, a.privateIP, b.privateIP, peeringDropPriority(16))
		requirePolicyDrop(t, stage, peeringDropPriority(16), "blackhole route in a subnet whose table has 0.0.0.0/0 to an IGW")
		require.Falsef(t, strings.Contains(stage, "outport = "), "the blackhole forwarded\n%s", stage)
	})

	harness.Step(t, "8. the gateway deletes once no attachment is live")
	deleteTransitGateway(t, c, tgw)
	require.Equal(t, "blackhole", routeState(t, c, a.rtbID, b.cidr))
}
