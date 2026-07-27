//go:build integration

package integration

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/require"
)

// defaultVPCInfo resolves the account's default VPC + its default subnet,
// created by StartDaemonLite's EnsureDefaultVPC call — the in-process
// equivalent of the E2E source's harness.EnsureDefaultVPC.
func defaultVPCInfo(t *testing.T, c *ec2.EC2) (vpcID, subnetID string) {
	t.Helper()
	vpcsOut, err := c.DescribeVpcs(&ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{{Name: aws.String("is-default"), Values: []*string{aws.String("true")}}},
	})
	require.NoError(t, err, "describe-vpcs is-default")
	require.NotEmpty(t, vpcsOut.Vpcs, "no default VPC found")
	vpcID = aws.StringValue(vpcsOut.Vpcs[0].VpcId)
	require.NotEmpty(t, vpcID, "default VpcId empty")

	subOut, err := c.DescribeSubnets(&ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{{Name: aws.String("vpc-id"), Values: []*string{aws.String(vpcID)}}},
	})
	require.NoError(t, err, "describe-subnets vpc-id=%s", vpcID)
	require.NotEmpty(t, subOut.Subnets, "no subnet found in default VPC %s", vpcID)
	subnetID = aws.StringValue(subOut.Subnets[0].SubnetId)
	require.NotEmpty(t, subnetID, "default SubnetId empty")
	return vpcID, subnetID
}

// createScratchVPC creates a VPC with the given CIDR and registers its deletion.
func createScratchVPC(t *testing.T, c *ec2.EC2, cidr string) string {
	t.Helper()
	out, err := c.CreateVpc(&ec2.CreateVpcInput{CidrBlock: aws.String(cidr)})
	require.NoError(t, err, "create-vpc %s", cidr)
	require.NotNil(t, out.Vpc, "create-vpc %s returned nil Vpc", cidr)
	id := aws.StringValue(out.Vpc.VpcId)
	require.NotEmpty(t, id, "VpcId empty for %s", cidr)
	t.Cleanup(func() {
		_, _ = c.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(id)})
	})
	return id
}

// createAttachedIGW creates an internet gateway, attaches it to vpcID, and
// registers detach+delete cleanup (LIFO-safe: detach runs before the owning
// VPC's delete).
func createAttachedIGW(t *testing.T, c *ec2.EC2, vpcID string) string {
	t.Helper()
	out, err := c.CreateInternetGateway(&ec2.CreateInternetGatewayInput{})
	require.NoError(t, err, "create-internet-gateway")
	require.NotNil(t, out.InternetGateway, "create-internet-gateway returned nil IGW")
	igwID := aws.StringValue(out.InternetGateway.InternetGatewayId)
	require.NotEmpty(t, igwID, "InternetGatewayId empty")
	_, err = c.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String(vpcID),
	})
	require.NoError(t, err, "attach-internet-gateway %s -> %s", igwID, vpcID)
	t.Cleanup(func() {
		_, _ = c.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
			InternetGatewayId: aws.String(igwID),
			VpcId:             aws.String(vpcID),
		})
		_, _ = c.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{
			InternetGatewayId: aws.String(igwID),
		})
	})
	return igwID
}

// routeGateway returns the GatewayId of the route matching destCidr in rtbID,
// failing the test if the route is absent.
func routeGateway(t *testing.T, c *ec2.EC2, rtbID, destCidr string) string {
	t.Helper()
	out, err := c.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
		RouteTableIds: []*string{aws.String(rtbID)},
	})
	require.NoError(t, err, "describe-route-tables %s", rtbID)
	require.NotEmpty(t, out.RouteTables, "route table %s not found", rtbID)
	for _, r := range out.RouteTables[0].Routes {
		if aws.StringValue(r.DestinationCidrBlock) == destCidr {
			return aws.StringValue(r.GatewayId)
		}
	}
	t.Fatalf("route %s not found in table %s", destCidr, rtbID)
	return ""
}

// TestRouteTableValidation is ported from tests/e2e/single/vpc_test.go
// runRouteTableValidation, driving the REAL routetable/service_impl.go (and
// vpc/service_impl.go for the default-VPC main table) over the harness's
// DaemonLite.
//
// Step1's IGW check remains a soft, non-fatal log: DaemonLite's default VPC
// (via VPCServiceImpl.EnsureDefaultVPC) has no IGW auto-attached — the live
// daemon's IGW auto-attach on account creation
// (daemon.ensureDefaultVPCInfrastructureFor) lives in the daemon package and
// is not replicated here — so this always takes the "no IGW attached"
// branch, exactly as the E2E source's comment anticipates for a
// private-only bootstrap.
func TestRouteTableValidation(t *testing.T) {
	gw := StartGateway(t)
	StartDaemonLite(t, gw)
	c := gw.EC2Client(t)

	vpcID, subnetID := defaultVPCInfo(t, c)

	// --- Step 1: Default VPC main route table -----------------------------
	var mainRTBID string
	t.Run("Step1_DefaultMainRouteTable", func(t *testing.T) {
		out, err := c.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
			Filters: []*ec2.Filter{
				{Name: aws.String("vpc-id"), Values: []*string{aws.String(vpcID)}},
				{Name: aws.String("association.main"), Values: []*string{aws.String("true")}},
			},
		})
		require.NoError(t, err, "describe-route-tables (main)")
		require.NotEmptyf(t, out.RouteTables,
			"no main route table found for default VPC %s", vpcID)
		mainRTBID = aws.StringValue(out.RouteTables[0].RouteTableId)
		require.NotEmpty(t, mainRTBID, "main RouteTableId empty")

		mainFlagSeen := false
		for _, a := range out.RouteTables[0].Associations {
			if aws.BoolValue(a.Main) {
				mainFlagSeen = true
				break
			}
		}
		require.Truef(t, mainFlagSeen,
			"main route table %s has no Association with Main=true", mainRTBID)

		localFound := false
		for _, r := range out.RouteTables[0].Routes {
			if aws.StringValue(r.GatewayId) == "local" {
				localFound = true
				break
			}
		}
		require.Truef(t, localFound,
			"main route table %s missing local route", mainRTBID)

		// IGW route (0.0.0.0/0 -> igw-...) — soft check, see doc comment above.
		igwRoute := ""
		for _, r := range out.RouteTables[0].Routes {
			if aws.StringValue(r.DestinationCidrBlock) == "0.0.0.0/0" {
				igwRoute = aws.StringValue(r.GatewayId)
				break
			}
		}
		if igwRoute == "" {
			t.Logf("note: default VPC main RT has no 0.0.0.0/0 route (no IGW attached)")
		}
	})

	if mainRTBID == "" {
		t.Fatalf("Step 1 did not resolve the main route table; cannot continue")
	}

	// --- Step 2: Custom route table CRUD lifecycle ------------------------
	t.Run("Step2_CustomRouteTableLifecycle", func(t *testing.T) {
		ctOut, err := c.CreateRouteTable(&ec2.CreateRouteTableInput{
			VpcId: aws.String(vpcID),
		})
		require.NoError(t, err, "create custom route table")
		require.NotNil(t, ctOut.RouteTable, "create-route-table returned nil RouteTable")
		customRTB := aws.StringValue(ctOut.RouteTable.RouteTableId)
		require.NotEmpty(t, customRTB, "custom RouteTableId empty")
		rtbDeleted := false
		t.Cleanup(func() {
			if !rtbDeleted {
				_, _ = c.DeleteRouteTable(&ec2.DeleteRouteTableInput{
					RouteTableId: aws.String(customRTB),
				})
			}
		})

		descCustom, err := c.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
			RouteTableIds: []*string{aws.String(customRTB)},
		})
		require.NoError(t, err, "describe custom route table")
		require.NotEmpty(t, descCustom.RouteTables, "custom RT not visible after create")
		customLocal := ""
		for _, r := range descCustom.RouteTables[0].Routes {
			if aws.StringValue(r.GatewayId) == "local" {
				customLocal = aws.StringValue(r.DestinationCidrBlock)
				break
			}
		}
		require.NotEmptyf(t, customLocal,
			"custom route table %s missing local route", customRTB)

		// Find an attached IGW for the default VPC so we can add a real
		// 0.0.0.0/0 route. None is attached in this tier (see doc comment),
		// so this always exercises the associate/disassociate + local-route
		// delete path only, same as the E2E source's no-IGW fallback.
		igwForVPC := ""
		igws, err := c.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{
			Filters: []*ec2.Filter{
				{Name: aws.String("attachment.vpc-id"), Values: []*string{aws.String(vpcID)}},
			},
		})
		require.NoError(t, err, "describe-internet-gateways")
		if len(igws.InternetGateways) > 0 {
			igwForVPC = aws.StringValue(igws.InternetGateways[0].InternetGatewayId)
		}

		routeAdded := false
		if igwForVPC != "" {
			_, err = c.CreateRoute(&ec2.CreateRouteInput{
				RouteTableId:         aws.String(customRTB),
				DestinationCidrBlock: aws.String("0.0.0.0/0"),
				GatewayId:            aws.String(igwForVPC),
			})
			require.NoError(t, err, "create-route 0.0.0.0/0 -> IGW (custom RT)")
			routeAdded = true
			t.Cleanup(func() {
				_, _ = c.DeleteRoute(&ec2.DeleteRouteInput{
					RouteTableId:         aws.String(customRTB),
					DestinationCidrBlock: aws.String("0.0.0.0/0"),
				})
			})
		}

		assocOut, err := c.AssociateRouteTable(&ec2.AssociateRouteTableInput{
			RouteTableId: aws.String(customRTB),
			SubnetId:     aws.String(subnetID),
		})
		require.NoError(t, err, "associate-route-table custom")
		assocID := aws.StringValue(assocOut.AssociationId)
		require.NotEmpty(t, assocID, "custom AssociationId empty")
		assocReleased := false
		t.Cleanup(func() {
			if !assocReleased {
				_, _ = c.DisassociateRouteTable(&ec2.DisassociateRouteTableInput{
					AssociationId: aws.String(assocID),
				})
			}
		})

		_, err = c.DisassociateRouteTable(&ec2.DisassociateRouteTableInput{
			AssociationId: aws.String(assocID),
		})
		require.NoError(t, err, "disassociate-route-table")
		assocReleased = true

		if routeAdded {
			_, err = c.DeleteRoute(&ec2.DeleteRouteInput{
				RouteTableId:         aws.String(customRTB),
				DestinationCidrBlock: aws.String("0.0.0.0/0"),
			})
			require.NoError(t, err, "delete-route 0.0.0.0/0")
		}

		_, err = c.DeleteRouteTable(&ec2.DeleteRouteTableInput{
			RouteTableId: aws.String(customRTB),
		})
		require.NoError(t, err, "delete-route-table")
		rtbDeleted = true
	})

	// --- Step 3: Error paths -----------------------------------------------
	t.Run("Step3_ErrorPaths", func(t *testing.T) {
		ctOut, err := c.CreateRouteTable(&ec2.CreateRouteTableInput{
			VpcId: aws.String(vpcID),
		})
		require.NoError(t, err, "create scratch RT")
		require.NotNil(t, ctOut.RouteTable, "create-route-table returned nil RouteTable")
		errRTB := aws.StringValue(ctOut.RouteTable.RouteTableId)
		require.NotEmpty(t, errRTB, "scratch RouteTableId empty")
		t.Cleanup(func() {
			_, _ = c.DeleteRouteTable(&ec2.DeleteRouteTableInput{
				RouteTableId: aws.String(errRTB),
			})
		})

		descScratch, err := c.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
			RouteTableIds: []*string{aws.String(errRTB)},
		})
		require.NoError(t, err, "describe scratch RT")
		require.NotEmpty(t, descScratch.RouteTables, "scratch RT not visible")
		scratchLocal := ""
		for _, r := range descScratch.RouteTables[0].Routes {
			if aws.StringValue(r.GatewayId) == "local" {
				scratchLocal = aws.StringValue(r.DestinationCidrBlock)
				break
			}
		}
		require.NotEmptyf(t, scratchLocal,
			"scratch RT %s missing local route", errRTB)

		// DeleteRoute on the local CIDR must be rejected. Assert *any* error
		// (matching the E2E source) rather than pin a code, since the
		// gateway might surface this as InvalidParameterValue,
		// OperationNotPermitted, or a custom "cannot delete local route" code.
		_, err = c.DeleteRoute(&ec2.DeleteRouteInput{
			RouteTableId:         aws.String(errRTB),
			DestinationCidrBlock: aws.String(scratchLocal),
		})
		require.Errorf(t, err,
			"delete-route on local route %s succeeded — must be rejected", scratchLocal)
		var aerr awserr.Error
		if e, ok := err.(awserr.Error); ok {
			aerr = e
			t.Logf("delete_local_err=%s", aerr.Code())
		}

		// DeleteRouteTable on the main RT must be rejected — same shape.
		_, err = c.DeleteRouteTable(&ec2.DeleteRouteTableInput{
			RouteTableId: aws.String(mainRTBID),
		})
		require.Errorf(t, err,
			"delete-route-table on main RT %s succeeded — must be rejected", mainRTBID)
		if e, ok := err.(awserr.Error); ok {
			t.Logf("delete_main_err=%s", e.Code())
		}
	})
}

// TestReplaceRouteConvergence is ported from tests/e2e/single/vpc_test.go
// runReplaceRouteConvergence, driving the REAL routetable/service_impl.go
// ReplaceRoute over the harness's DaemonLite. Validates ReplaceRoute's
// control-plane contract: a route installed against one IGW target
// re-points to a different IGW, with the route record converging to the new
// gateway. Then walks the negative matrix (no-such-route, local-route,
// missing-target, unknown-IGW, cross-VPC IGW) so each rejection path stays
// pinned to its AWS error code — identical assertions to the E2E source.
//
// V1 scope note: ReplaceRoute updates the route record but publishes no vpc
// event, so it does NOT reprogram any datapath — there is no datapath to
// assert here (no OVN in this tier either way); the contract under test is
// control-plane convergence only, same as the E2E source.
func TestReplaceRouteConvergence(t *testing.T) {
	gw := StartGateway(t)
	StartDaemonLite(t, gw)
	c := gw.EC2Client(t)

	// --- Primary VPC + two attached IGWs -----------------------------------
	vpcID := createScratchVPC(t, c, "10.77.0.0/16")
	igw1 := createAttachedIGW(t, c, vpcID)
	igw2 := createAttachedIGW(t, c, vpcID)

	// --- Secondary VPC + its own IGW (cross-VPC rejection target) ----------
	otherVPCID := createScratchVPC(t, c, "10.88.0.0/16")
	igwOther := createAttachedIGW(t, c, otherVPCID)

	// --- Custom route table in the primary VPC -----------------------------
	rtOut, err := c.CreateRouteTable(&ec2.CreateRouteTableInput{VpcId: aws.String(vpcID)})
	require.NoError(t, err, "create-route-table")
	require.NotNil(t, rtOut.RouteTable, "create-route-table returned nil RouteTable")
	rtbID := aws.StringValue(rtOut.RouteTable.RouteTableId)
	require.NotEmpty(t, rtbID, "RouteTableId empty")
	t.Cleanup(func() {
		_, _ = c.DeleteRouteTable(&ec2.DeleteRouteTableInput{RouteTableId: aws.String(rtbID)})
	})

	const dest = "192.0.2.0/24"

	// --- CreateRoute -> igw1 -----------------------------------------------
	_, err = c.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String(dest),
		GatewayId:            aws.String(igw1),
	})
	require.NoError(t, err, "create-route %s -> igw1", dest)
	require.Equalf(t, igw1, routeGateway(t, c, rtbID, dest),
		"route %s should target igw1 after create", dest)

	// --- ReplaceRoute -> igw2 (genuine target convergence) -----------------
	_, err = c.ReplaceRoute(&ec2.ReplaceRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String(dest),
		GatewayId:            aws.String(igw2),
	})
	require.NoError(t, err, "replace-route %s -> igw2", dest)
	require.Equalf(t, igw2, routeGateway(t, c, rtbID, dest),
		"route %s did not converge to igw2 after replace-route", dest)

	// --- Negative matrix ---------------------------------------------------
	// Unknown destination CIDR — route doesn't exist in the table.
	_, err = c.ReplaceRoute(&ec2.ReplaceRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("198.51.100.0/24"),
		GatewayId:            aws.String(igw2),
	})
	requireAWSErrorCode(t, err, "InvalidRoute.NotFound")

	// Local route is immutable.
	_, err = c.ReplaceRoute(&ec2.ReplaceRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("10.77.0.0/16"),
		GatewayId:            aws.String(igw2),
	})
	requireAWSErrorCode(t, err, "InvalidParameterValue")

	// Missing target — V1 requires a GatewayId.
	_, err = c.ReplaceRoute(&ec2.ReplaceRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String(dest),
	})
	requireAWSErrorCode(t, err, "InvalidParameterValue")

	// Unknown IGW target.
	_, err = c.ReplaceRoute(&ec2.ReplaceRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String(dest),
		GatewayId:            aws.String("igw-00000000000000000"),
	})
	requireAWSErrorCode(t, err, "InvalidInternetGatewayID.NotFound")

	// IGW attached to a different VPC than the route table.
	_, err = c.ReplaceRoute(&ec2.ReplaceRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String(dest),
		GatewayId:            aws.String(igwOther),
	})
	requireAWSErrorCode(t, err, "InvalidParameterValue")

	// Each rejection left the route untouched at igw2.
	require.Equalf(t, igw2, routeGateway(t, c, rtbID, dest),
		"route %s drifted off igw2 after the rejection matrix", dest)
}
