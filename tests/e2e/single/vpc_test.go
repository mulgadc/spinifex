//go:build e2e

package single

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
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

	tgt := harness.SSHTarget{User: "ubuntu", Host: pubIP, Port: 22, KeyPath: keyPath}

	harness.Step(t, "ssh id (smoke)")
	idOut := runSSH(t, tgt, "id")
	require.Containsf(t, idOut, "ubuntu", "ssh id did not report ubuntu\n%s", idOut)

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
