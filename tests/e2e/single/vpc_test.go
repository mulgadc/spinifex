//go:build e2e

package single

import (
	"errors"
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

// runVPCSubnetE2E ports run-e2e.sh Phase 8b (~2747-3496).
//
// The bash driver layers Phase 8b on top of the *default* VPC + IGW that
// admin init prebakes, then leans on Phase 8d / 8e to share the public and
// private instances it launches. The Go port instead stands up an entire
// non-default VPC from scratch so the test owns every resource it touches:
//
//   - CreateVpc (10.99.0.0/16)
//   - CreateSubnet public (10.99.1.0/24) + ModifySubnetAttribute MapPublicIpOnLaunch=true
//   - CreateSubnet private (10.99.2.0/24)
//   - CreateInternetGateway + AttachInternetGateway
//   - CreateRouteTable + AssociateRouteTable (public subnet) + CreateRoute 0.0.0.0/0 -> IGW
//   - RunInstances in public subnet, wait running, assert PublicIpAddress
//   - SSH in via the public IP and verify external connectivity (ping 8.8.8.8)
//
// Skipped outside pool/external mode because dev_networking single-node mode
// has no IPAM for the OVN bridge and the bastion would never get a routable
// public IP. The bash version skips the same way via HAS_EXTERNAL.
//
// Cleanup runs LIFO via t.Cleanup so a failure at any step still tears the
// VPC graph down in the correct order: instance -> wait terminated ->
// disassociate route table -> delete route(s) -> delete route table ->
// detach IGW -> delete IGW -> delete subnets -> delete VPC.
func runVPCSubnetE2E(t *testing.T, fix *Fixture) {
	if !fix.PoolMode {
		t.Skip("Phase 8b requires pool-mode networking")
	}
	harness.Phase(t, "Single — VPC Public/Private Subnet E2E")

	amiID := needAMI(t, fix)
	instType, _ := needInstanceTypeArch(t, fix)
	keyName, keyPath := needKeyPair(t, fix)

	c := fix.AWS

	// --- VPC ---------------------------------------------------------------
	harness.Step(t, "create-vpc 10.99.0.0/16")
	vpcOut, err := c.EC2.CreateVpc(&ec2.CreateVpcInput{
		CidrBlock: aws.String("10.99.0.0/16"),
	})
	require.NoError(t, err, "create-vpc")
	require.NotNil(t, vpcOut.Vpc, "create-vpc returned nil Vpc")
	vpcID := aws.StringValue(vpcOut.Vpc.VpcId)
	require.NotEmpty(t, vpcID, "VpcId empty")
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(vpcID)})
	})
	harness.Detail(t, "vpc", vpcID, "cidr", "10.99.0.0/16")

	// --- Public subnet -----------------------------------------------------
	harness.Step(t, "create-subnet 10.99.1.0/24 (public)")
	pubSubOut, err := c.EC2.CreateSubnet(&ec2.CreateSubnetInput{
		VpcId:     aws.String(vpcID),
		CidrBlock: aws.String("10.99.1.0/24"),
	})
	require.NoError(t, err, "create public subnet")
	require.NotNil(t, pubSubOut.Subnet, "create public subnet returned nil Subnet")
	pubSubnetID := aws.StringValue(pubSubOut.Subnet.SubnetId)
	require.NotEmpty(t, pubSubnetID, "public SubnetId empty")
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String(pubSubnetID)})
	})

	// Flip MapPublicIpOnLaunch=true so RunInstances picks up a public IP
	// without an explicit AssociatePublicIpAddress on the NIC spec. The bash
	// driver relies on the default VPC having this attribute pre-set; we set
	// it here explicitly so the test is self-contained.
	harness.Step(t, "modify-subnet-attribute MapPublicIpOnLaunch=true %s", pubSubnetID)
	_, err = c.EC2.ModifySubnetAttribute(&ec2.ModifySubnetAttributeInput{
		SubnetId:            aws.String(pubSubnetID),
		MapPublicIpOnLaunch: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	})
	require.NoError(t, err, "modify-subnet-attribute MapPublicIpOnLaunch")
	harness.Detail(t, "pub_subnet", pubSubnetID, "map_public_ip", "true")

	// --- Private subnet ----------------------------------------------------
	harness.Step(t, "create-subnet 10.99.2.0/24 (private)")
	privSubOut, err := c.EC2.CreateSubnet(&ec2.CreateSubnetInput{
		VpcId:     aws.String(vpcID),
		CidrBlock: aws.String("10.99.2.0/24"),
	})
	require.NoError(t, err, "create private subnet")
	require.NotNil(t, privSubOut.Subnet, "create private subnet returned nil Subnet")
	privSubnetID := aws.StringValue(privSubOut.Subnet.SubnetId)
	require.NotEmpty(t, privSubnetID, "private SubnetId empty")
	require.Falsef(t, aws.BoolValue(privSubOut.Subnet.MapPublicIpOnLaunch),
		"private subnet MapPublicIpOnLaunch must default to false (got %v)",
		aws.BoolValue(privSubOut.Subnet.MapPublicIpOnLaunch))
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String(privSubnetID)})
	})
	harness.Detail(t, "priv_subnet", privSubnetID, "map_public_ip", "false")

	// --- Internet gateway --------------------------------------------------
	harness.Step(t, "create-internet-gateway")
	igwOut, err := c.EC2.CreateInternetGateway(&ec2.CreateInternetGatewayInput{})
	require.NoError(t, err, "create-internet-gateway")
	require.NotNil(t, igwOut.InternetGateway, "create-internet-gateway returned nil InternetGateway")
	igwID := aws.StringValue(igwOut.InternetGateway.InternetGatewayId)
	require.NotEmpty(t, igwID, "InternetGatewayId empty")
	igwDetached := false
	t.Cleanup(func() {
		if !igwDetached {
			_, _ = c.EC2.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
				InternetGatewayId: aws.String(igwID),
				VpcId:             aws.String(vpcID),
			})
		}
		_, _ = c.EC2.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{
			InternetGatewayId: aws.String(igwID),
		})
	})

	harness.Step(t, "attach-internet-gateway %s -> %s", igwID, vpcID)
	_, err = c.EC2.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String(vpcID),
	})
	require.NoError(t, err, "attach-internet-gateway")
	harness.Detail(t, "igw", igwID)

	// --- Route table -------------------------------------------------------
	harness.Step(t, "create-route-table (vpc=%s)", vpcID)
	rtOut, err := c.EC2.CreateRouteTable(&ec2.CreateRouteTableInput{
		VpcId: aws.String(vpcID),
	})
	require.NoError(t, err, "create-route-table")
	require.NotNil(t, rtOut.RouteTable, "create-route-table returned nil RouteTable")
	rtbID := aws.StringValue(rtOut.RouteTable.RouteTableId)
	require.NotEmpty(t, rtbID, "RouteTableId empty")
	rtbDeleted := false
	t.Cleanup(func() {
		if !rtbDeleted {
			_, _ = c.EC2.DeleteRouteTable(&ec2.DeleteRouteTableInput{
				RouteTableId: aws.String(rtbID),
			})
		}
	})

	harness.Step(t, "associate-route-table %s <- %s", rtbID, pubSubnetID)
	assocOut, err := c.EC2.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(rtbID),
		SubnetId:     aws.String(pubSubnetID),
	})
	require.NoError(t, err, "associate-route-table")
	assocID := aws.StringValue(assocOut.AssociationId)
	require.NotEmpty(t, assocID, "AssociationId empty")
	assocReleased := false
	t.Cleanup(func() {
		if !assocReleased {
			_, _ = c.EC2.DisassociateRouteTable(&ec2.DisassociateRouteTableInput{
				AssociationId: aws.String(assocID),
			})
		}
	})

	harness.Step(t, "create-route 0.0.0.0/0 -> %s", igwID)
	_, err = c.EC2.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String(igwID),
	})
	require.NoError(t, err, "create-route 0.0.0.0/0 -> IGW")
	routeDeleted := false
	t.Cleanup(func() {
		if !routeDeleted {
			_, _ = c.EC2.DeleteRoute(&ec2.DeleteRouteInput{
				RouteTableId:         aws.String(rtbID),
				DestinationCidrBlock: aws.String("0.0.0.0/0"),
			})
		}
	})
	harness.Detail(t, "rtb", rtbID, "assoc", assocID, "route", "0.0.0.0/0->"+igwID)

	// --- Security group ----------------------------------------------------
	// Fresh VPC's auto-created default SG denies all ingress. Authorize
	// tcp/22 from anywhere so the SSH probe below can complete — the bash
	// driver relies on the *default* VPC's default SG having been pre-baked
	// with this rule by admin init, but a newly-created VPC has not.
	// Without this step Phase 8b SSH times out via an ACL drop between OVN
	// tables 13 (DNAT) and 17 (NAT commit) even when every datapath barrier
	// is satisfied.
	harness.Step(t, "describe-security-groups (default) vpc=%s", vpcID)
	sgOut, err := c.EC2.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("vpc-id"), Values: []*string{aws.String(vpcID)}},
			{Name: aws.String("group-name"), Values: []*string{aws.String("default")}},
		},
	})
	require.NoError(t, err, "describe-security-groups default")
	require.NotEmpty(t, sgOut.SecurityGroups, "no default SG for vpc=%s", vpcID)
	defaultSGID := aws.StringValue(sgOut.SecurityGroups[0].GroupId)
	require.NotEmpty(t, defaultSGID, "default SG GroupId empty")
	harness.AuthorizeSSHIngress(t, c, defaultSGID)
	harness.Detail(t, "default_sg", defaultSGID, "ingress", "tcp/22<-0.0.0.0/0")

	// --- Public instance ---------------------------------------------------
	harness.Step(t, "run-instances ami=%s subnet=%s", amiID, pubSubnetID)
	runOut, err := c.EC2.RunInstances(&ec2.RunInstancesInput{
		ImageId:      aws.String(amiID),
		InstanceType: aws.String(instType),
		KeyName:      aws.String(keyName),
		SubnetId:     aws.String(pubSubnetID),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	})
	require.NoError(t, err, "run-instances public")
	require.NotEmpty(t, runOut.Instances, "run-instances returned no Instances")
	instID := aws.StringValue(runOut.Instances[0].InstanceId)
	require.NotEmpty(t, instID, "InstanceId empty")
	t.Cleanup(func() {
		_, _ = c.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
			InstanceIds: []*string{aws.String(instID)},
		})
		// Wait for terminated so the subnet / VPC can drop. Best-effort —
		// we're already unwinding so swallow errors.
		_ = waitForInstanceStateSoft(c, instID, "terminated", 5*time.Minute)
	})
	harness.Detail(t, "instance", instID)

	inst := harness.WaitForInstanceState(t, c, instID, "running")
	pubIP := aws.StringValue(inst.PublicIpAddress)
	privIP := aws.StringValue(inst.PrivateIpAddress)
	require.NotEmptyf(t, pubIP,
		"public-subnet instance %s has no PublicIpAddress (MapPublicIpOnLaunch failed?)", instID)
	require.NotEmpty(t, privIP, "PrivateIpAddress empty")
	harness.Detail(t, "public_ip", pubIP, "private_ip", privIP)

	// --- SSH + external connectivity ---------------------------------------
	// Use the non-fatal probe so we can dump VPC/IGW datapath diagnostics
	// before Fatal. fresh-VPC unreachability beyond the budget is a real
	// product bug, not test flake.
	if !trySSHReady(pubIP, 22, keyPath, sshReadyBudget) {
		harness.DumpVPCFlowDiagnostics(t, c, instID,
			fmt.Sprintf("Phase 8b SSH timeout — vpc=%s igw=%s pub=%s", vpcID, igwID, pubIP),
			harness.VPCDiagnosticsOpts{
				ExternalIP:  pubIP,
				LogicalIP:   privIP,
				ArtifactDir: fix.ArtifactDir(t),
			})
		t.Fatalf("SSH handshake %s:22 never completed within %s (see diagnostics above)", pubIP, sshReadyBudget)
	}

	tgt := harness.SSHTarget{User: "ec2-user", Host: pubIP, Port: 22, KeyPath: keyPath}

	harness.Step(t, "ssh id (smoke)")
	idOut := runSSH(t, tgt, "id")
	require.Containsf(t, idOut, "ec2-user", "ssh id did not report ec2-user\n%s", idOut)

	// Verify external connectivity through the IGW + 0.0.0.0/0 route. ping
	// is treated the same way bash treats it: a soft check that prints a
	// warning rather than failing the suite — WAN egress depends on the
	// hosting network and isn't part of the EC2 surface contract.
	harness.Step(t, "verify external connectivity via IGW (ping 8.8.8.8)")
	pingOK := false
	harness.EventuallyErr(t, func() error {
		out, perr := runSSHQuiet(tgt, "ping -c 2 -W 5 8.8.8.8 >/dev/null 2>&1 && echo yes || echo no")
		if perr != nil {
			return fmt.Errorf("ssh ping: %w (out=%q)", perr, out)
		}
		if strings.TrimSpace(out) == "yes" {
			pingOK = true
			return nil
		}
		return fmt.Errorf("ping did not succeed (out=%q)", strings.TrimSpace(out))
	}, 90*time.Second, 5*time.Second)
	if pingOK {
		harness.Detail(t, "external_ping", "ok")
	} else {
		// EventuallyErr would have fatalled if it never reached pingOK=true;
		// this branch is defensive (covers a future refactor that swaps in
		// a non-fatal poll). Keep the bash-style warning so triagers still
		// see the WAN egress signal.
		t.Logf("WARN: external ping inconclusive — may depend on WAN gateway")
	}
}

// runRouteTableValidation ports run-e2e.sh Phase 8c (~2626-2745).
//
// Validates route-table CRUD + invariants against the *default* VPC's main
// route table (always runs — no pool-mode gating). Three sub-tests:
//
//	Step1_DefaultMainRouteTable — DescribeRouteTables filter by vpc-id +
//	    association.main asserts the local route is present. The IGW route
//	    is verified only when the default VPC has an attached IGW (some
//	    bootstraps skip it on private-only fixtures).
//	Step2_CustomRouteTableLifecycle — CreateRouteTable, verify the implicit
//	    local route, AssociateRouteTable to a default subnet, DisassociateRouteTable,
//	    DeleteRoute on a manually-added route, DeleteRouteTable.
//	Step3_ErrorPaths — DeleteRoute on the local route + DeleteRouteTable on
//	    the main route table must both fail.
//
// Each step registers its own cleanup so a partial run still releases the
// scratch route table / association regardless of which assertion fired.
func runRouteTableValidation(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — Route Table Validation")

	c := fix.AWS

	// Default VPC + subnet + SG are owned by the harness Ensure* memo, so
	// repeated calls across phases (and post-migration: across sibling
	// tests) hit the same cached IDs without re-discovering.
	def := harness.EnsureDefaultVPC(t, fix.Harness)
	require.NotEmpty(t, def.VPCID, "default VPC ID required")
	require.NotEmpty(t, def.SubnetID, "default subnet ID required")
	harness.Detail(t, "vpc", def.VPCID, "subnet", def.SubnetID)

	// --- Step 1: Default VPC main route table -----------------------------
	var mainRTBID string
	t.Run("Step1_DefaultMainRouteTable", func(t *testing.T) {
		harness.Step(t, "describe-route-tables filter=vpc-id,association.main")
		out, err := c.EC2.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
			Filters: []*ec2.Filter{
				{Name: aws.String("vpc-id"), Values: []*string{aws.String(def.VPCID)}},
				{Name: aws.String("association.main"), Values: []*string{aws.String("true")}},
			},
		})
		require.NoError(t, err, "describe-route-tables (main)")
		require.NotEmptyf(t, out.RouteTables,
			"no main route table found for default VPC %s", def.VPCID)
		mainRTBID = aws.StringValue(out.RouteTables[0].RouteTableId)
		require.NotEmpty(t, mainRTBID, "main RouteTableId empty")
		harness.Detail(t, "main_rtb", mainRTBID)

		// GetRouteTableAssociations equivalent: confirm at least one
		// association of the main RT is flagged Main=true. The aws-sdk-go
		// surface folds this into RouteTable.Associations[].Main, so a
		// separate API call isn't needed — but assert it explicitly so a
		// regression that drops the Main flag would fail here, not silently.
		mainFlagSeen := false
		for _, a := range out.RouteTables[0].Associations {
			if aws.BoolValue(a.Main) {
				mainFlagSeen = true
				break
			}
		}
		require.Truef(t, mainFlagSeen,
			"main route table %s has no Association with Main=true", mainRTBID)

		// Local route is mandatory on every route table — the gateway
		// auto-creates it at CreateRouteTable / CreateVpc time.
		localFound := false
		for _, r := range out.RouteTables[0].Routes {
			if aws.StringValue(r.GatewayId) == "local" {
				localFound = true
				harness.Detail(t, "local_route", aws.StringValue(r.DestinationCidrBlock))
				break
			}
		}
		require.Truef(t, localFound,
			"main route table %s missing local route", mainRTBID)

		// IGW route (0.0.0.0/0 -> igw-...) — soft check. Default VPCs that
		// admin init wires with an IGW must have it; private-only bootstraps
		// legitimately don't, so just log when missing rather than fail.
		igwRoute := ""
		for _, r := range out.RouteTables[0].Routes {
			if aws.StringValue(r.DestinationCidrBlock) == "0.0.0.0/0" {
				igwRoute = aws.StringValue(r.GatewayId)
				break
			}
		}
		if igwRoute != "" {
			harness.Detail(t, "default_route", "0.0.0.0/0->"+igwRoute)
		} else {
			t.Logf("note: default VPC main RT has no 0.0.0.0/0 route (no IGW attached)")
		}
	})

	// Bail if Step 1 didn't seat mainRTBID — Steps 2/3 dereference it.
	if mainRTBID == "" {
		t.Fatalf("Step 1 did not resolve the main route table; cannot continue")
	}

	// --- Step 2: Custom route table CRUD lifecycle ------------------------
	t.Run("Step2_CustomRouteTableLifecycle", func(t *testing.T) {
		harness.Step(t, "create-route-table (vpc=%s)", def.VPCID)
		ctOut, err := c.EC2.CreateRouteTable(&ec2.CreateRouteTableInput{
			VpcId: aws.String(def.VPCID),
		})
		require.NoError(t, err, "create custom route table")
		require.NotNil(t, ctOut.RouteTable, "create-route-table returned nil RouteTable")
		customRTB := aws.StringValue(ctOut.RouteTable.RouteTableId)
		require.NotEmpty(t, customRTB, "custom RouteTableId empty")
		rtbDeleted := false
		t.Cleanup(func() {
			if !rtbDeleted {
				_, _ = c.EC2.DeleteRouteTable(&ec2.DeleteRouteTableInput{
					RouteTableId: aws.String(customRTB),
				})
			}
		})
		harness.Detail(t, "custom_rtb", customRTB)

		// Local route must be present on the freshly-created table.
		descCustom, err := c.EC2.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
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
		harness.Detail(t, "custom_local_route", customLocal)

		// Find an attached IGW for the default VPC so we can add a real
		// 0.0.0.0/0 route. If none is attached the test still exercises
		// associate/disassociate + delete on the local route only.
		igwForVPC := ""
		igws, err := c.EC2.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{
			Filters: []*ec2.Filter{
				{Name: aws.String("attachment.vpc-id"), Values: []*string{aws.String(def.VPCID)}},
			},
		})
		require.NoError(t, err, "describe-internet-gateways")
		if len(igws.InternetGateways) > 0 {
			igwForVPC = aws.StringValue(igws.InternetGateways[0].InternetGatewayId)
			harness.Detail(t, "vpc_igw", igwForVPC)
		}

		routeAdded := false
		if igwForVPC != "" {
			harness.Step(t, "create-route 0.0.0.0/0 -> %s", igwForVPC)
			_, err = c.EC2.CreateRoute(&ec2.CreateRouteInput{
				RouteTableId:         aws.String(customRTB),
				DestinationCidrBlock: aws.String("0.0.0.0/0"),
				GatewayId:            aws.String(igwForVPC),
			})
			require.NoError(t, err, "create-route 0.0.0.0/0 -> IGW (custom RT)")
			routeAdded = true
			t.Cleanup(func() {
				_, _ = c.EC2.DeleteRoute(&ec2.DeleteRouteInput{
					RouteTableId:         aws.String(customRTB),
					DestinationCidrBlock: aws.String("0.0.0.0/0"),
				})
			})
		}

		// Associate with the default subnet, then disassociate. The bash
		// driver uses `default-for-az` to find a subnet; def.SubnetID
		// is already that subnet (DiscoverDefaultVPC prefers DefaultForAz).
		harness.Step(t, "associate-route-table %s <- %s", customRTB, def.SubnetID)
		assocOut, err := c.EC2.AssociateRouteTable(&ec2.AssociateRouteTableInput{
			RouteTableId: aws.String(customRTB),
			SubnetId:     aws.String(def.SubnetID),
		})
		require.NoError(t, err, "associate-route-table custom")
		assocID := aws.StringValue(assocOut.AssociationId)
		require.NotEmpty(t, assocID, "custom AssociationId empty")
		assocReleased := false
		t.Cleanup(func() {
			if !assocReleased {
				_, _ = c.EC2.DisassociateRouteTable(&ec2.DisassociateRouteTableInput{
					AssociationId: aws.String(assocID),
				})
			}
		})
		harness.Detail(t, "custom_assoc", assocID)

		harness.Step(t, "disassociate-route-table %s", assocID)
		_, err = c.EC2.DisassociateRouteTable(&ec2.DisassociateRouteTableInput{
			AssociationId: aws.String(assocID),
		})
		require.NoError(t, err, "disassociate-route-table")
		assocReleased = true

		if routeAdded {
			harness.Step(t, "delete-route 0.0.0.0/0")
			_, err = c.EC2.DeleteRoute(&ec2.DeleteRouteInput{
				RouteTableId:         aws.String(customRTB),
				DestinationCidrBlock: aws.String("0.0.0.0/0"),
			})
			require.NoError(t, err, "delete-route 0.0.0.0/0")
		}

		harness.Step(t, "delete-route-table %s", customRTB)
		_, err = c.EC2.DeleteRouteTable(&ec2.DeleteRouteTableInput{
			RouteTableId: aws.String(customRTB),
		})
		require.NoError(t, err, "delete-route-table")
		rtbDeleted = true
	})

	// --- Step 3: Error paths -----------------------------------------------
	t.Run("Step3_ErrorPaths", func(t *testing.T) {
		// Scratch RT for the "cannot delete local route" assertion. The bash
		// version uses a fresh table so the failure doesn't poison the main
		// table or the Step 2 lifecycle table.
		harness.Step(t, "create-route-table (scratch for error paths)")
		ctOut, err := c.EC2.CreateRouteTable(&ec2.CreateRouteTableInput{
			VpcId: aws.String(def.VPCID),
		})
		require.NoError(t, err, "create scratch RT")
		require.NotNil(t, ctOut.RouteTable, "create-route-table returned nil RouteTable")
		errRTB := aws.StringValue(ctOut.RouteTable.RouteTableId)
		require.NotEmpty(t, errRTB, "scratch RouteTableId empty")
		t.Cleanup(func() {
			_, _ = c.EC2.DeleteRouteTable(&ec2.DeleteRouteTableInput{
				RouteTableId: aws.String(errRTB),
			})
		})

		// Discover the scratch table's local CIDR (the VPC's CIDR block).
		descScratch, err := c.EC2.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
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

		// DeleteRoute on the local CIDR must be rejected. Bash just asserts
		// non-zero exit; we assert *any* error here rather than pin a code,
		// because the gateway might surface this as InvalidParameterValue,
		// OperationNotPermitted, or a custom "cannot delete local route" code.
		// If the gateway settles on a single code in future we can tighten this
		// to harness.AssertAWSError.
		harness.Step(t, "delete-route local=%s (must fail)", scratchLocal)
		_, err = c.EC2.DeleteRoute(&ec2.DeleteRouteInput{
			RouteTableId:         aws.String(errRTB),
			DestinationCidrBlock: aws.String(scratchLocal),
		})
		require.Errorf(t, err,
			"delete-route on local route %s succeeded — must be rejected", scratchLocal)
		// Sanity: the rejection should be an AWS-shaped error, not a transport
		// error. Surface the code in the test log so a future tightening to
		// AssertAWSError has a known target.
		var aerr awserr.Error
		if errors.As(err, &aerr) {
			harness.Detail(t, "delete_local_err", aerr.Code())
		}

		// DeleteRouteTable on the main RT must be rejected — same shape.
		harness.Step(t, "delete-route-table %s (main, must fail)", mainRTBID)
		_, err = c.EC2.DeleteRouteTable(&ec2.DeleteRouteTableInput{
			RouteTableId: aws.String(mainRTBID),
		})
		require.Errorf(t, err,
			"delete-route-table on main RT %s succeeded — must be rejected", mainRTBID)
		if errors.As(err, &aerr) {
			harness.Detail(t, "delete_main_err", aerr.Code())
		}
	})
}

// runReplaceRouteConvergence validates ReplaceRoute's control-plane contract:
// a route installed against one IGW target re-points to a different IGW, with
// the route record converging to the new gateway. It then walks the negative
// matrix (no-such-route, local-route, missing-target, unknown-IGW, cross-VPC
// IGW) so each rejection path stays pinned to its AWS error code.
//
// V1 scope note: ReplaceRoute updates the route record but publishes no vpc
// event, so it does NOT reprogram the OVN datapath. There is no datapath to
// assert here — the contract under test is control-plane convergence only.
// When ReplaceRoute begins emitting add/delete-igw-route events, extend this
// with a guest-egress assertion mirroring runNATGateway.
//
// Owns two scratch VPCs + three IGWs end to end, so it never touches the
// singleton or default VPC and is safe in the parallel bucket. No pool mode or
// OVN needed — every step is pure KV control plane.
func runReplaceRouteConvergence(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — ReplaceRoute Convergence (control plane)")

	c := fix.AWS

	// --- Primary VPC + two attached IGWs -----------------------------------
	vpcID := createScratchVPC(t, c, "10.77.0.0/16")
	igw1 := createAttachedIGW(t, c, vpcID)
	igw2 := createAttachedIGW(t, c, vpcID)

	// --- Secondary VPC + its own IGW (cross-VPC rejection target) ----------
	otherVPCID := createScratchVPC(t, c, "10.88.0.0/16")
	igwOther := createAttachedIGW(t, c, otherVPCID)

	// --- Custom route table in the primary VPC -----------------------------
	harness.Step(t, "create-route-table (vpc=%s)", vpcID)
	rtOut, err := c.EC2.CreateRouteTable(&ec2.CreateRouteTableInput{VpcId: aws.String(vpcID)})
	require.NoError(t, err, "create-route-table")
	require.NotNil(t, rtOut.RouteTable, "create-route-table returned nil RouteTable")
	rtbID := aws.StringValue(rtOut.RouteTable.RouteTableId)
	require.NotEmpty(t, rtbID, "RouteTableId empty")
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteRouteTable(&ec2.DeleteRouteTableInput{RouteTableId: aws.String(rtbID)})
	})

	const dest = "192.0.2.0/24"

	// --- CreateRoute -> igw1 -----------------------------------------------
	harness.Step(t, "create-route %s -> %s", dest, igw1)
	_, err = c.EC2.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String(dest),
		GatewayId:            aws.String(igw1),
	})
	require.NoError(t, err, "create-route %s -> igw1", dest)
	require.Equalf(t, igw1, routeGateway(t, c, rtbID, dest),
		"route %s should target igw1 after create", dest)

	// --- ReplaceRoute -> igw2 (genuine target convergence) -----------------
	harness.Step(t, "replace-route %s -> %s", dest, igw2)
	_, err = c.EC2.ReplaceRoute(&ec2.ReplaceRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String(dest),
		GatewayId:            aws.String(igw2),
	})
	require.NoError(t, err, "replace-route %s -> igw2", dest)
	require.Equalf(t, igw2, routeGateway(t, c, rtbID, dest),
		"route %s did not converge to igw2 after replace-route", dest)
	harness.Detail(t, "convergence", dest+": "+igw1+" -> "+igw2)

	// --- Negative matrix ---------------------------------------------------
	// Unknown destination CIDR — route doesn't exist in the table.
	harness.Step(t, "replace-route on absent route (must fail)")
	harness.ExpectError(t, "InvalidRoute.NotFound", func() error {
		_, e := c.EC2.ReplaceRoute(&ec2.ReplaceRouteInput{
			RouteTableId:         aws.String(rtbID),
			DestinationCidrBlock: aws.String("198.51.100.0/24"),
			GatewayId:            aws.String(igw2),
		})
		return e
	})

	// Local route is immutable.
	harness.Step(t, "replace-route on local route (must fail)")
	harness.ExpectError(t, "InvalidParameterValue", func() error {
		_, e := c.EC2.ReplaceRoute(&ec2.ReplaceRouteInput{
			RouteTableId:         aws.String(rtbID),
			DestinationCidrBlock: aws.String("10.77.0.0/16"),
			GatewayId:            aws.String(igw2),
		})
		return e
	})

	// Missing target — V1 requires a GatewayId.
	harness.Step(t, "replace-route with no target (must fail)")
	harness.ExpectError(t, "InvalidParameterValue", func() error {
		_, e := c.EC2.ReplaceRoute(&ec2.ReplaceRouteInput{
			RouteTableId:         aws.String(rtbID),
			DestinationCidrBlock: aws.String(dest),
		})
		return e
	})

	// Unknown IGW target.
	harness.Step(t, "replace-route to non-existent IGW (must fail)")
	harness.ExpectError(t, "InvalidInternetGatewayID.NotFound", func() error {
		_, e := c.EC2.ReplaceRoute(&ec2.ReplaceRouteInput{
			RouteTableId:         aws.String(rtbID),
			DestinationCidrBlock: aws.String(dest),
			GatewayId:            aws.String("igw-00000000000000000"),
		})
		return e
	})

	// IGW attached to a different VPC than the route table.
	harness.Step(t, "replace-route to cross-VPC IGW %s (must fail)", igwOther)
	harness.ExpectError(t, "InvalidParameterValue", func() error {
		_, e := c.EC2.ReplaceRoute(&ec2.ReplaceRouteInput{
			RouteTableId:         aws.String(rtbID),
			DestinationCidrBlock: aws.String(dest),
			GatewayId:            aws.String(igwOther),
		})
		return e
	})

	// Each rejection left the route untouched at igw2.
	require.Equalf(t, igw2, routeGateway(t, c, rtbID, dest),
		"route %s drifted off igw2 after the rejection matrix", dest)
}

// createScratchVPC creates a VPC with the given CIDR and registers its deletion.
func createScratchVPC(t *testing.T, c *harness.AWSClient, cidr string) string {
	t.Helper()
	harness.Step(t, "create-vpc %s", cidr)
	out, err := c.EC2.CreateVpc(&ec2.CreateVpcInput{CidrBlock: aws.String(cidr)})
	require.NoError(t, err, "create-vpc %s", cidr)
	require.NotNil(t, out.Vpc, "create-vpc %s returned nil Vpc", cidr)
	id := aws.StringValue(out.Vpc.VpcId)
	require.NotEmpty(t, id, "VpcId empty for %s", cidr)
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(id)})
	})
	return id
}

// createAttachedIGW creates an internet gateway, attaches it to vpcID, and
// registers detach+delete cleanup (LIFO-safe: detach runs before the owning
// VPC's delete).
func createAttachedIGW(t *testing.T, c *harness.AWSClient, vpcID string) string {
	t.Helper()
	out, err := c.EC2.CreateInternetGateway(&ec2.CreateInternetGatewayInput{})
	require.NoError(t, err, "create-internet-gateway")
	require.NotNil(t, out.InternetGateway, "create-internet-gateway returned nil IGW")
	igwID := aws.StringValue(out.InternetGateway.InternetGatewayId)
	require.NotEmpty(t, igwID, "InternetGatewayId empty")
	_, err = c.EC2.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String(vpcID),
	})
	require.NoError(t, err, "attach-internet-gateway %s -> %s", igwID, vpcID)
	t.Cleanup(func() {
		_, _ = c.EC2.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
			InternetGatewayId: aws.String(igwID),
			VpcId:             aws.String(vpcID),
		})
		_, _ = c.EC2.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{
			InternetGatewayId: aws.String(igwID),
		})
	})
	return igwID
}

// routeGateway returns the GatewayId of the route matching destCidr in rtbID,
// failing the test if the route is absent.
func routeGateway(t *testing.T, c *harness.AWSClient, rtbID, destCidr string) string {
	t.Helper()
	out, err := c.EC2.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
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
