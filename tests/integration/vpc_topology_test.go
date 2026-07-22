//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/stretchr/testify/require"
)

// startVPCDStack wires the full API-to-OVN chain: the real gateway router, a
// real vpcd event consumer over a real OVN NB DB, and the real VPC service
// impl answering the ec2.* subjects.
//
// Order matters. StartVPCDLite runs first because StartDaemonLite's
// EnsureDefaultVPC synchronously requests vpc.create-sg, which now has a real
// responder rather than a canned ack.
func startVPCDStack(t *testing.T) (*VPCDLite, *ec2.EC2) {
	t.Helper()

	gw := StartGateway(t)
	vpcd := StartVPCDLite(t, gw)
	StartDaemonLite(t, gw, WithRealVPCD())

	return vpcd, gw.EC2Client(t)
}

// TestVPCD_VPCLifecycleReachesOVN asserts CreateVpc/DeleteVpc travel the whole
// chain — SigV4 request, gateway, ec2.CreateVpc, VPCServiceImpl, vpc.create,
// subscriber, topology manager, OVSDB transaction — and land as a logical
// router carrying the VPC's identity.
//
// Row contents beyond identity are network/reconcile's scenario tests to
// assert; what is unique here is that the wire between the two services
// carries the event at all.
func TestVPCD_VPCLifecycleReachesOVN(t *testing.T) {
	vpcd, c := startVPCDStack(t)

	vpcID := createScratchVPC(t, c, "10.42.0.0/16")
	router := topology.VPCRouter(vpcID)

	awaitNB(t, "logical router "+router, func(ctx context.Context) error {
		lr, err := vpcd.OVN.GetLogicalRouter(ctx, router)
		if err != nil {
			return err
		}
		if got := lr.ExternalIDs["spinifex:vpc_id"]; got != vpcID {
			return fmt.Errorf("spinifex:vpc_id = %q, want %q", got, vpcID)
		}
		if got := lr.ExternalIDs["spinifex:cidr"]; got != "10.42.0.0/16" {
			return fmt.Errorf("spinifex:cidr = %q, want 10.42.0.0/16", got)
		}
		return nil
	})

	_, err := c.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(vpcID)})
	require.NoError(t, err, "delete-vpc %s", vpcID)

	awaitNB(t, "logical router "+router+" removed", func(ctx context.Context) error {
		if _, err := vpcd.OVN.GetLogicalRouter(ctx, router); err == nil {
			return fmt.Errorf("logical router %s still present", router)
		}
		return nil
	})
}

// TestVPCD_SubnetLifecycleReachesOVN asserts CreateSubnet/DeleteSubnet reach
// OVN as a logical switch attached to the owning VPC's router.
func TestVPCD_SubnetLifecycleReachesOVN(t *testing.T) {
	vpcd, c := startVPCDStack(t)

	vpcID := createScratchVPC(t, c, "10.43.0.0/16")
	out, err := c.CreateSubnet(&ec2.CreateSubnetInput{
		VpcId:     aws.String(vpcID),
		CidrBlock: aws.String("10.43.1.0/24"),
	})
	require.NoError(t, err, "create-subnet in %s", vpcID)
	require.NotNil(t, out.Subnet, "create-subnet returned nil Subnet")
	subnetID := aws.StringValue(out.Subnet.SubnetId)
	require.NotEmpty(t, subnetID, "SubnetId empty")

	sw := topology.SubnetSwitch(subnetID)
	awaitNB(t, "logical switch "+sw, func(ctx context.Context) error {
		ls, err := vpcd.OVN.GetLogicalSwitch(ctx, sw)
		if err != nil {
			return err
		}
		if got := ls.ExternalIDs["spinifex:subnet_id"]; got != subnetID {
			return fmt.Errorf("spinifex:subnet_id = %q, want %q", got, subnetID)
		}
		return nil
	})

	// The subnet's default gateway terminates on an LRP of the VPC router, so
	// its presence is what proves the switch was joined to the right VPC
	// rather than merely created.
	lrp := topology.SubnetRouterPort(subnetID)
	awaitNB(t, "logical router port "+lrp, func(ctx context.Context) error {
		_, err := vpcd.OVN.GetLogicalRouterPort(ctx, lrp)
		return err
	})

	_, err = c.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String(subnetID)})
	require.NoError(t, err, "delete-subnet %s", subnetID)

	awaitNB(t, "logical switch "+sw+" removed", func(ctx context.Context) error {
		if _, err := vpcd.OVN.GetLogicalSwitch(ctx, sw); err == nil {
			return fmt.Errorf("logical switch %s still present", sw)
		}
		return nil
	})
}
