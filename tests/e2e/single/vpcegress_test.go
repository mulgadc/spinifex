//go:build e2e

package single

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// vpcEgressNATRounds returns how many NAT Gateway up/down flip cycles the
// round loop repeats against the same private instance, overridable via
// SPINIFEX_VPCEGRESS_NAT_ROUNDS. Defaults to 1, matching the former
// standalone NAT Gateway test's single-pass behaviour; bump it for a soak
// run that repeatedly provisions and tears down the gateway to catch
// route/SNAT propagation flakiness a single cycle would not surface.
func vpcEgressNATRounds() int {
	return envPositiveIntOr("SPINIFEX_VPCEGRESS_NAT_ROUNDS", 1)
}

// runVPCEgressPaths merges four checks that used to boot their own VPC
// apiece — normal-order public/private subnet egress, the route-before-subnet
// ordering regression guard, NAT Gateway egress, and instance Elastic IP
// association — around one scenario-owned VPC and its two guests, cutting
// five boots to three.
//
// The three ways a guest in this VPC reaches (or is reached from) the
// internet — a direct IGW route on its own subnet, a NAT Gateway fronting a
// private subnet, and an Elastic IP flipped onto a running instance — share
// one public guest and one private guest instead of each paying for its own.
// The fourth stage, RouteBeforeSubnet, is a regression guard for an ordering
// edge case in the first path (the IGW route can be installed on a VPC's
// main route table before any subnet exists) and needs a second VPC to
// reproduce that ordering, so it is not foldable into the shared guests
// below without losing the precondition it exists to prove.
//
// Stage order and gating:
//   - VPC A (10.99.0.0/16, public + private subnet, explicit route table
//     associated to the public subnet after it exists) and its public guest
//     are unconditional prerequisites; any failure here is fatal in the
//     ordinary Go-test sense (require), matching the originals.
//   - PublicSubnetEgress proves the public guest's own IGW egress works.
//     Every later stage that touches this same guest — the NAT Gateway
//     bastion hop and the EIP flip — depends on its SSH datapath already
//     being healthy, so PublicSubnetEgress's failure aborts the rest of the
//     scenario rather than let two more stages time out for the same reason.
//   - RouteBeforeSubnet builds its own second VPC end to end (IGW route
//     installed on the main route table before the subnet that implicitly
//     joins it even exists) and never shares state with any other stage, so
//     it registers its own cleanup on its own subtest rather than the
//     scenario's — its resources are freed the moment the stage ends instead
//     of lingering to the end of the whole scenario. This ordering
//     precondition is the one piece of coverage here that cannot be merged
//     away: the explicit route table this scenario uses for the public
//     subnet above exercises a different code path (CreateRouteTable +
//     AssociateRouteTable) than the implicit main-route-table membership
//     this stage depends on, so collapsing the two would silently stop
//     testing whichever path got dropped.
//   - The NAT Gateway private guest launches into VPC A's already-existing
//     private subnet — no separate VPC or subnet needed — and every NAT
//     stage reaches it by hopping through the public guest over SSH, reusing
//     it as the bastion rather than booting a dedicated one. NATBaseline
//     (no internet before any NAT Gateway exists) runs once; the up/down
//     flip that follows repeats for vpcEgressNATRounds() rounds (default 1)
//     so a soak run can catch propagation flakiness a single cycle would
//     not surface. Each round's NATGatewayDown only runs if that round's
//     NATGatewayUp succeeded — tearing down a gateway that was never
//     fully provisioned would produce misleading failures rather than
//     useful signal.
//   - EIPFlip runs last and reuses the same public guest as the EIP subject.
//     Associating an Elastic IP rewrites the instance's public IP, which
//     would corrupt every stage above that still expects the guest to
//     answer on its original address — ordering it last is what lets this
//     scenario reuse the guest at all instead of needing a disposable one.
func runVPCEgressPaths(t *testing.T, fix *Fixture) {
	if !fix.PoolMode {
		t.Skip("VPC egress paths scenario requires pool-mode networking")
	}
	harness.Phase(t, "Single — Three ways in and out: public subnet, NAT Gateway, and Elastic IP egress around one VPC")

	c := fix.AWS
	amiID := needAMI(t, fix)
	instType, _ := needInstanceTypeArch(t, fix)
	keyName, keyPath := needKeyPair(t, fix)

	// --- VPC A: public + private subnet, explicit route table, normal ordering (subnet before route) ---

	harness.Step(t, "create-vpc 10.99.0.0/16")
	vpcOut, err := c.EC2.CreateVpc(&ec2.CreateVpcInput{CidrBlock: aws.String("10.99.0.0/16")})
	require.NoError(t, err, "create-vpc")
	require.NotNil(t, vpcOut.Vpc, "create-vpc returned nil Vpc")
	vpcID := aws.StringValue(vpcOut.Vpc.VpcId)
	require.NotEmpty(t, vpcID, "VpcId empty")
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(vpcID)})
	})
	harness.Detail(t, "vpc", vpcID, "cidr", "10.99.0.0/16")

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

	harness.Step(t, "modify-subnet-attribute MapPublicIpOnLaunch=true %s", pubSubnetID)
	_, err = c.EC2.ModifySubnetAttribute(&ec2.ModifySubnetAttributeInput{
		SubnetId:            aws.String(pubSubnetID),
		MapPublicIpOnLaunch: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	})
	require.NoError(t, err, "modify-subnet-attribute MapPublicIpOnLaunch")
	harness.Detail(t, "pub_subnet", pubSubnetID, "map_public_ip", "true")

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

	// Fresh VPC's auto-created default SG denies all ingress. Authorize
	// tcp/22 from anywhere so every guest launched into VPC A (public and
	// private alike) can be reached via the bastion hop below.
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

	// --- Public guest: PublicSubnetEgress, then the NAT bastion hop, then (last) EIPFlip ---

	harness.Step(t, "run-instances ami=%s subnet=%s (public)", amiID, pubSubnetID)
	pubRunOut, err := c.EC2.RunInstances(&ec2.RunInstancesInput{
		ImageId:      aws.String(amiID),
		InstanceType: aws.String(instType),
		KeyName:      aws.String(keyName),
		SubnetId:     aws.String(pubSubnetID),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	})
	require.NoError(t, err, "run-instances public")
	require.NotEmpty(t, pubRunOut.Instances, "run-instances public returned no Instances")
	pubInstanceID := aws.StringValue(pubRunOut.Instances[0].InstanceId)
	require.NotEmpty(t, pubInstanceID, "public InstanceId empty")
	t.Cleanup(func() {
		_, _ = c.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
			InstanceIds: []*string{aws.String(pubInstanceID)},
		})
		_ = waitForInstanceStateSoft(c, pubInstanceID, "terminated", 5*time.Minute)
	})
	harness.Detail(t, "instance", pubInstanceID)

	pubInst := harness.WaitForInstanceState(t, c, pubInstanceID, "running")
	pubIP := aws.StringValue(pubInst.PublicIpAddress)
	pubPrivIP := aws.StringValue(pubInst.PrivateIpAddress)
	require.NotEmptyf(t, pubIP,
		"public-subnet instance %s has no PublicIpAddress (MapPublicIpOnLaunch failed?)", pubInstanceID)
	require.NotEmpty(t, pubPrivIP, "PrivateIpAddress empty")
	harness.Detail(t, "public_ip", pubIP, "private_ip", pubPrivIP)

	pubOK := t.Run("PublicSubnetEgress", func(t *testing.T) {
		if !trySSHReady(pubIP, 22, keyPath, sshReadyBudget) {
			harness.DumpVPCFlowDiagnostics(t, c, pubInstanceID,
				fmt.Sprintf("PublicSubnetEgress SSH timeout — vpc=%s igw=%s pub=%s", vpcID, igwID, pubIP),
				harness.VPCDiagnosticsOpts{
					ExternalIP:  pubIP,
					LogicalIP:   pubPrivIP,
					ArtifactDir: fix.ArtifactDir(t),
				})
			t.Fatalf("SSH handshake %s:22 never completed within %s (see diagnostics above)", pubIP, sshReadyBudget)
		}

		tgt := harness.SSHTarget{User: "ubuntu", Host: pubIP, Port: 22, KeyPath: keyPath}

		harness.Step(t, "ssh id (smoke)")
		idOut := runSSH(t, tgt, "id")
		require.Containsf(t, idOut, "ubuntu", "ssh id did not report ubuntu\n%s", idOut)

		// Verify external connectivity through the IGW + 0.0.0.0/0 route. ping
		// is a soft check: WAN egress depends on the hosting network and isn't
		// part of the EC2 surface contract.
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
			t.Logf("WARN: external ping inconclusive — may depend on WAN gateway")
		}
	})
	if !pubOK {
		t.Fatalf("PublicSubnetEgress stage failed; skipping every later stage that reuses this guest as the NAT bastion / EIP subject")
	}

	bastionTgt := harness.SSHTarget{User: "ubuntu", Host: pubIP, Port: 22, KeyPath: keyPath}

	// --- RouteBeforeSubnet: its own second VPC, fully self-contained ---

	t.Run("RouteBeforeSubnet", func(t *testing.T) {
		az := needAZ(t, fix)
		const vpcCIDR = "10.231.0.0/16"
		const subnetCIDR = "10.231.1.0/24"

		vOut, err := c.EC2.CreateVpc(&ec2.CreateVpcInput{CidrBlock: aws.String(vpcCIDR)})
		require.NoError(t, err, "create-vpc")
		require.NotNil(t, vOut.Vpc, "create-vpc returned nil Vpc")
		vID := aws.StringValue(vOut.Vpc.VpcId)
		require.NotEmpty(t, vID, "VpcId empty")
		t.Cleanup(func() { _, _ = c.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(vID)}) })

		iOut, err := c.EC2.CreateInternetGateway(&ec2.CreateInternetGatewayInput{})
		require.NoError(t, err, "create-internet-gateway")
		require.NotNil(t, iOut.InternetGateway, "create-internet-gateway returned nil InternetGateway")
		iID := aws.StringValue(iOut.InternetGateway.InternetGatewayId)
		require.NotEmpty(t, iID, "InternetGatewayId empty")
		t.Cleanup(func() {
			_, _ = c.EC2.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
				InternetGatewayId: aws.String(iID), VpcId: aws.String(vID),
			})
			_, _ = c.EC2.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{
				InternetGatewayId: aws.String(iID),
			})
		})
		_, err = c.EC2.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
			InternetGatewayId: aws.String(iID), VpcId: aws.String(vID),
		})
		require.NoError(t, err, "attach-internet-gateway")

		mainRT := mainRouteTableID(t, c, vID)
		harness.Detail(t, "vpc", vID, "igw", iID, "main_rtb", mainRT)

		// Route FIRST, on the main RT, before the subnet exists.
		harness.Step(t, "create-route 0.0.0.0/0 -> %s on main RT %s (before subnet)", iID, mainRT)
		_, err = c.EC2.CreateRoute(&ec2.CreateRouteInput{
			RouteTableId:         aws.String(mainRT),
			DestinationCidrBlock: aws.String("0.0.0.0/0"),
			GatewayId:            aws.String(iID),
		})
		require.NoError(t, err, "create-route 0.0.0.0/0 -> IGW on main RT")
		t.Cleanup(func() {
			_, _ = c.EC2.DeleteRoute(&ec2.DeleteRouteInput{
				RouteTableId:         aws.String(mainRT),
				DestinationCidrBlock: aws.String("0.0.0.0/0"),
			})
		})

		// Subnet AFTER the route (implicit main-RT membership).
		harness.Step(t, "create-subnet %s (implicit main RT, after IGW route)", subnetCIDR)
		sOut, err := c.EC2.CreateSubnet(&ec2.CreateSubnetInput{
			VpcId:            aws.String(vID),
			CidrBlock:        aws.String(subnetCIDR),
			AvailabilityZone: aws.String(az),
		})
		require.NoError(t, err, "create-subnet")
		require.NotNil(t, sOut.Subnet, "create-subnet returned nil Subnet")
		sID := aws.StringValue(sOut.Subnet.SubnetId)
		require.NotEmpty(t, sID, "SubnetId empty")
		t.Cleanup(func() { _, _ = c.EC2.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String(sID)}) })

		_, err = c.EC2.ModifySubnetAttribute(&ec2.ModifySubnetAttributeInput{
			SubnetId:            aws.String(sID),
			MapPublicIpOnLaunch: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
		})
		require.NoError(t, err, "modify-subnet-attribute MapPublicIpOnLaunch on %s", sID)

		// Open SG so routing is provably the only barrier.
		sgID := harness.EnsureSG(t, fix.Harness, vID, "vpcegress-routebeforesubnet-sg")
		harness.AuthorizeSSHIngress(t, c, sgID)
		harness.Detail(t, "subnet", sID, "cidr", subnetCIDR, "sg", sgID)

		instanceID := launchBaselineInstance(t, fix, amiID, instType, keyName, sID, []string{sgID})
		rbPubIP := instancePublicIP(t, fix, instanceID)
		harness.Detail(t, "instance", instanceID, "public_ip", rbPubIP)

		harness.Step(t, "expecting external SSH to instance in fresh-VPC public subnet")
		if !trySSHReady(rbPubIP, 22, keyPath, sshReadyBudget) {
			harness.DumpVPCFlowDiagnostics(t, c, instanceID,
				fmt.Sprintf("RouteBeforeSubnet SSH timeout — vpc=%s igw=%s pub=%s", vID, iID, rbPubIP),
				harness.VPCDiagnosticsOpts{
					ExternalIP:  rbPubIP,
					LogicalIP:   instancePrivateIP(t, fix, instanceID),
					ArtifactDir: fix.ArtifactDir(t),
				})
			t.Fatalf("tcp/22 to %s in fresh-VPC subnet %s never became reachable — the "+
				"per-subnet IGW egress policy was not installed for a subnet created "+
				"after the main-RT IGW route existed", rbPubIP, sID)
		}

		tgt := harness.SSHTarget{User: "ubuntu", Host: rbPubIP, Port: 22, KeyPath: keyPath}
		idOut := runSSH(t, tgt, "id")
		assert.Containsf(t, idOut, "ubuntu", "ssh id in fresh-VPC subnet\n%s", idOut)
	})

	// --- NAT Gateway: private guest in VPC A's private subnet, bastion-hop via the public guest ---

	harness.Step(t, "run-instances private (VPC A private subnet)")
	privRunOut, err := c.EC2.RunInstances(&ec2.RunInstancesInput{
		ImageId:      aws.String(amiID),
		InstanceType: aws.String(instType),
		KeyName:      aws.String(keyName),
		SubnetId:     aws.String(privSubnetID),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	})
	require.NoError(t, err, "run-instances private")
	require.NotEmpty(t, privRunOut.Instances, "run-instances private returned no Instances")
	privInstanceID := aws.StringValue(privRunOut.Instances[0].InstanceId)
	require.NotEmpty(t, privInstanceID, "private InstanceId empty")
	t.Cleanup(func() {
		_, _ = c.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
			InstanceIds: []*string{aws.String(privInstanceID)},
		})
		_ = waitForInstanceStateSoft(c, privInstanceID, "terminated", 5*time.Minute)
	})

	privInst := harness.WaitForInstanceState(t, c, privInstanceID, "running")
	privIP := aws.StringValue(privInst.PrivateIpAddress)
	require.NotEmptyf(t, privIP, "private instance %s has no PrivateIpAddress", privInstanceID)
	require.Emptyf(t, aws.StringValue(privInst.PublicIpAddress),
		"private instance %s unexpectedly has a public IP (got %q)",
		privInstanceID, aws.StringValue(privInst.PublicIpAddress))
	harness.Detail(t, "private_instance", privInstanceID, "private_ip", privIP)

	// Copy the keypair to the bastion so it can hop into the private guest.
	harness.Step(t, "scp keypair -> bastion:/tmp/key.pem")
	scpKey(t, keyPath, pubIP)
	_ = runSSH(t, bastionTgt, "chmod 600 /tmp/key.pem")

	privProbe := fmt.Sprintf(
		"ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null "+
			"-o LogLevel=ERROR -o ConnectTimeout=10 -o BatchMode=yes "+
			"-i /tmp/key.pem ubuntu@%s hostname",
		privIP,
	)
	harness.Step(t, "wait for private SSH via bastion")
	harness.EventuallyErr(t, func() error {
		out, rerr := runSSHQuiet(bastionTgt, privProbe)
		if rerr != nil {
			return fmt.Errorf("bastion->priv hostname: %w (out=%q)", rerr, out)
		}
		if strings.TrimSpace(out) == "" {
			return fmt.Errorf("empty hostname response from private VM")
		}
		return nil
	}, 3*time.Minute, 5*time.Second)

	pingCmd := func() string {
		return fmt.Sprintf(
			"ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null "+
				"-o LogLevel=ERROR -o ConnectTimeout=10 -o BatchMode=yes "+
				"-i /tmp/key.pem ubuntu@%s 'ping -c 1 -W 3 8.8.8.8'",
			privIP,
		)
	}

	t.Run("NATBaseline", func(t *testing.T) {
		harness.Step(t, "baseline: private VM has no internet")
		if _, err := runSSHQuiet(bastionTgt, pingCmd()); err == nil {
			t.Fatalf("baseline ping unexpectedly succeeded — private VM has internet without NAT GW")
		}
	})

	rounds := vpcEgressNATRounds()
	for round := 1; round <= rounds; round++ {
		round := round
		t.Run(fmt.Sprintf("Round%d", round), func(t *testing.T) {
			roundT := t

			var natGWID, natAllocID, natPubIP, natRTBID, natAssocID string
			eipReleased := false
			natDeleted := false
			natRTBDeleted := false
			natAssocReleased := false
			natRouteDeleted := false

			natUpOK := t.Run("NATGatewayUp", func(t *testing.T) {
				harness.Step(t, "allocate-address (NAT EIP)")
				eipOut, err := c.EC2.AllocateAddress(&ec2.AllocateAddressInput{
					Domain: aws.String("vpc"),
				})
				require.NoError(t, err, "allocate-address vpc")
				natAllocID = aws.StringValue(eipOut.AllocationId)
				natPubIP = aws.StringValue(eipOut.PublicIp)
				require.NotEmpty(t, natAllocID, "AllocationId empty")
				roundT.Cleanup(func() {
					if eipReleased {
						return
					}
					_, _ = c.EC2.ReleaseAddress(&ec2.ReleaseAddressInput{
						AllocationId: aws.String(natAllocID),
					})
				})
				harness.Detail(t, "eip", natPubIP, "alloc", natAllocID)

				harness.Step(t, "create-nat-gateway in %s", pubSubnetID)
				natOut, err := c.EC2.CreateNatGateway(&ec2.CreateNatGatewayInput{
					SubnetId:     aws.String(pubSubnetID),
					AllocationId: aws.String(natAllocID),
				})
				require.NoError(t, err, "create-nat-gateway")
				require.NotNil(t, natOut.NatGateway, "create-nat-gateway returned nil NatGateway")
				natGWID = aws.StringValue(natOut.NatGateway.NatGatewayId)
				require.NotEmpty(t, natGWID, "NatGatewayId empty")
				roundT.Cleanup(func() {
					if natDeleted {
						return
					}
					_, _ = c.EC2.DeleteNatGateway(&ec2.DeleteNatGatewayInput{
						NatGatewayId: aws.String(natGWID),
					})
					_ = waitForNATGatewayStateSoft(c, natGWID, "deleted", 5*time.Minute)
				})
				harness.Detail(t, "nat_gw", natGWID)

				waitForNATGatewayState(t, c, natGWID, "available")

				// Route table -> associate -> CreateRoute. The association
				// must exist before the route so the SNAT publication fires
				// off it, mirroring the original test's ordering.
				harness.Step(t, "create-route-table")
				rtOut, err := c.EC2.CreateRouteTable(&ec2.CreateRouteTableInput{
					VpcId: aws.String(vpcID),
				})
				require.NoError(t, err, "create-route-table")
				require.NotNil(t, rtOut.RouteTable, "create-route-table returned nil RouteTable")
				natRTBID = aws.StringValue(rtOut.RouteTable.RouteTableId)
				require.NotEmpty(t, natRTBID, "RouteTableId empty")
				roundT.Cleanup(func() {
					if natRTBDeleted {
						return
					}
					_, _ = c.EC2.DeleteRouteTable(&ec2.DeleteRouteTableInput{
						RouteTableId: aws.String(natRTBID),
					})
				})

				harness.Step(t, "associate-route-table %s <- %s", natRTBID, privSubnetID)
				assocOut, err := c.EC2.AssociateRouteTable(&ec2.AssociateRouteTableInput{
					RouteTableId: aws.String(natRTBID),
					SubnetId:     aws.String(privSubnetID),
				})
				require.NoError(t, err, "associate-route-table")
				natAssocID = aws.StringValue(assocOut.AssociationId)
				require.NotEmpty(t, natAssocID, "AssociationId empty")
				roundT.Cleanup(func() {
					if natAssocReleased {
						return
					}
					_, _ = c.EC2.DisassociateRouteTable(&ec2.DisassociateRouteTableInput{
						AssociationId: aws.String(natAssocID),
					})
				})

				harness.Step(t, "create-route 0.0.0.0/0 -> %s", natGWID)
				_, err = c.EC2.CreateRoute(&ec2.CreateRouteInput{
					RouteTableId:         aws.String(natRTBID),
					DestinationCidrBlock: aws.String("0.0.0.0/0"),
					NatGatewayId:         aws.String(natGWID),
				})
				require.NoError(t, err, "create-route 0.0.0.0/0")
				roundT.Cleanup(func() {
					if natRouteDeleted {
						return
					}
					_, _ = c.EC2.DeleteRoute(&ec2.DeleteRouteInput{
						RouteTableId:         aws.String(natRTBID),
						DestinationCidrBlock: aws.String("0.0.0.0/0"),
					})
				})

				// OVN needs a beat to install datapath flows after SNAT
				// publishes. Snapshot the SNAT datapath on failure so the
				// bundle carries OVN/OVS state to root-cause the loss.
				harness.OnFailure(t, func() {
					harness.DumpVPCFlowDiagnostics(t, c, privInstanceID,
						fmt.Sprintf("NAT GW egress — nat_gw=%s external_ip=%s logical_ip=%s", natGWID, natPubIP, privIP),
						harness.VPCDiagnosticsOpts{
							ExternalIP:  natPubIP,
							LogicalIP:   privIP,
							ArtifactDir: fix.ArtifactDir(t),
						})
				})
				harness.Step(t, "verify private VM reaches 8.8.8.8 via NAT GW")
				harness.EventuallyErr(t, func() error {
					out, perr := runSSHQuiet(bastionTgt, pingCmd())
					if perr != nil {
						return fmt.Errorf("ping via NAT GW: %w (out=%q)", perr, out)
					}
					return nil
				}, 2*time.Minute, 5*time.Second)
			})

			if !natUpOK {
				t.Logf("round %d: NATGatewayUp failed; skipping NATGatewayDown since there is nothing fully provisioned to tear down", round)
				return
			}

			t.Run("NATGatewayDown", func(t *testing.T) {
				// Mirror the create-time ordering in reverse: delete
				// nat-gateway -> disassociate route table -> delete route ->
				// delete route table -> release address. Each step flips the
				// corresponding latch so the safety-net roundT.Cleanup
				// callbacks above become no-ops on the success path.
				harness.Step(t, "delete-nat-gateway %s", natGWID)
				_, err := c.EC2.DeleteNatGateway(&ec2.DeleteNatGatewayInput{
					NatGatewayId: aws.String(natGWID),
				})
				require.NoError(t, err, "delete-nat-gateway")

				_, err = c.EC2.DisassociateRouteTable(&ec2.DisassociateRouteTableInput{
					AssociationId: aws.String(natAssocID),
				})
				require.NoError(t, err, "disassociate-route-table")
				natAssocReleased = true

				_, err = c.EC2.DeleteRoute(&ec2.DeleteRouteInput{
					RouteTableId:         aws.String(natRTBID),
					DestinationCidrBlock: aws.String("0.0.0.0/0"),
				})
				require.NoError(t, err, "delete-route")
				natRouteDeleted = true

				_, err = c.EC2.DeleteRouteTable(&ec2.DeleteRouteTableInput{
					RouteTableId: aws.String(natRTBID),
				})
				require.NoError(t, err, "delete-route-table")
				natRTBDeleted = true

				waitForNATGatewayState(t, c, natGWID, "deleted")
				natDeleted = true

				_, err = c.EC2.ReleaseAddress(&ec2.ReleaseAddressInput{
					AllocationId: aws.String(natAllocID),
				})
				require.NoError(t, err, "release-address")
				eipReleased = true

				// OVN datapath can briefly cache the old SNAT flow, so poll
				// for the loss rather than asserting immediately.
				harness.Step(t, "verify private VM lost internet after NAT GW teardown")
				harness.EventuallyErr(t, func() error {
					if _, perr := runSSHQuiet(bastionTgt, pingCmd()); perr == nil {
						return fmt.Errorf("ping still succeeding after NAT GW deletion")
					}
					return nil
				}, 90*time.Second, 5*time.Second)
			})
		})
	}

	// --- EIPFlip: last, on the same public guest ---

	t.Run("EIPFlip", func(t *testing.T) {
		harness.SkipIfNoOVN(t)

		harness.Step(t, "allocate-address (vpc)")
		eipOut, err := c.EC2.AllocateAddress(&ec2.AllocateAddressInput{Domain: aws.String("vpc")})
		require.NoError(t, err, "allocate-address")
		allocID := aws.StringValue(eipOut.AllocationId)
		eipIP := aws.StringValue(eipOut.PublicIp)
		require.NotEmpty(t, allocID, "AllocationId empty")
		require.NotEmpty(t, eipIP, "EIP PublicIp empty")
		eipReleased := false
		t.Cleanup(func() {
			if eipReleased {
				return
			}
			_, _ = c.EC2.ReleaseAddress(&ec2.ReleaseAddressInput{AllocationId: aws.String(allocID)})
		})
		harness.Detail(t, "eip", eipIP, "alloc", allocID)

		harness.Step(t, "associate-address %s -> %s", eipIP, pubInstanceID)
		assocOut, err := c.EC2.AssociateAddress(&ec2.AssociateAddressInput{
			AllocationId: aws.String(allocID),
			InstanceId:   aws.String(pubInstanceID),
		})
		require.NoError(t, err, "associate-address")
		eipAssocID := aws.StringValue(assocOut.AssociationId)
		require.NotEmpty(t, eipAssocID, "AssociationId empty")
		disassociated := false
		t.Cleanup(func() {
			if disassociated {
				return
			}
			_, _ = c.EC2.DisassociateAddress(&ec2.DisassociateAddressInput{AssociationId: aws.String(eipAssocID)})
		})
		harness.Detail(t, "assoc", eipAssocID)

		// OVN needs a beat to install the DNAT/SNAT flows after the
		// association publishes; poll the SSH handshake rather than
		// asserting immediately.
		harness.Step(t, "ssh to guest via EIP %s", eipIP)
		if !trySSHReady(eipIP, 22, keyPath, sshReadyBudget) {
			harness.DumpVPCFlowDiagnostics(t, c, pubInstanceID,
				fmt.Sprintf("EIP SSH timeout — eip=%s instance=%s", eipIP, pubInstanceID),
				harness.VPCDiagnosticsOpts{
					ExternalIP:  eipIP,
					LogicalIP:   pubPrivIP,
					ArtifactDir: fix.ArtifactDir(t),
				})
			t.Fatalf("guest unreachable via EIP %s within %s (see diagnostics above)", eipIP, sshReadyBudget)
		}
		tgt := harness.SSHTarget{User: "ubuntu", Host: eipIP, Port: 22, KeyPath: keyPath}
		idOut := runSSH(t, tgt, "id")
		require.Containsf(t, idOut, "ubuntu", "ssh via EIP id did not report ubuntu\n%s", idOut)
		harness.Detail(t, "datapath", "eip_reachable_ok")

		harness.Step(t, "disassociate-address %s", eipAssocID)
		_, err = c.EC2.DisassociateAddress(&ec2.DisassociateAddressInput{AssociationId: aws.String(eipAssocID)})
		require.NoError(t, err, "disassociate-address")
		disassociated = true

		// The EIP no longer maps to the guest, but DNAT teardown can lag and
		// a single transient SSH error must not be read as teardown —
		// require several consecutive unreachable probes before declaring
		// success.
		harness.Step(t, "verify guest unreachable via EIP %s after disassociate", eipIP)
		const wantUnreachable = 3
		consecutive := 0
		harness.EventuallyErr(t, func() error {
			if _, sErr := runSSHQuiet(tgt, "true"); sErr == nil {
				consecutive = 0
				return fmt.Errorf("EIP %s still reaches guest after disassociate", eipIP)
			}
			if consecutive++; consecutive < wantUnreachable {
				return fmt.Errorf("EIP %s unreachable %d/%d — confirming it is not a transient blip",
					eipIP, consecutive, wantUnreachable)
			}
			return nil
		}, 90*time.Second, 5*time.Second)

		// Rule out an instance-death false pass: the unreachability must be
		// the EIP teardown, not the subject VM having gone away mid-poll.
		harness.WaitForInstanceState(t, c, pubInstanceID, "running", harness.WithTimeout(30*time.Second))
		harness.Detail(t, "datapath", "eip_unreachable_after_disassociate_ok")

		harness.Step(t, "release-address %s", allocID)
		_, err = c.EC2.ReleaseAddress(&ec2.ReleaseAddressInput{AllocationId: aws.String(allocID)})
		require.NoError(t, err, "release-address")
		eipReleased = true
	})
}

// waitForNATGatewayState polls DescribeNatGateways until State == target.
func waitForNATGatewayState(t *testing.T, c *harness.AWSClient, id, target string) {
	t.Helper()
	var lastState string
	harness.EventuallyErr(t, func() error {
		out, err := c.EC2.DescribeNatGateways(&ec2.DescribeNatGatewaysInput{
			NatGatewayIds: []*string{aws.String(id)},
		})
		if err != nil {
			return fmt.Errorf("describe-nat-gateways %s: %w", id, err)
		}
		if len(out.NatGateways) == 0 {
			// "deleted" is a terminal state that DescribeNatGateways can
			// stop returning once the record is reaped — treat as success
			// only when that's what the caller wanted.
			if target == "deleted" {
				return nil
			}
			return fmt.Errorf("%s not found", id)
		}
		lastState = aws.StringValue(out.NatGateways[0].State)
		if lastState == target {
			return nil
		}
		if lastState == "failed" {
			msg := aws.StringValue(out.NatGateways[0].FailureMessage)
			return fmt.Errorf("%s entered failed state: %s", id, msg)
		}
		return fmt.Errorf("%s state=%s want=%s", id, lastState, target)
	}, 5*time.Minute, 2*time.Second)
	t.Logf("nat gateway %s reached state %s", id, target)
}

// waitForNATGatewayStateSoft is the cleanup-time variant: never calls
// t.Fatal, just polls best-effort with a caller-supplied timeout.
func waitForNATGatewayStateSoft(c *harness.AWSClient, id, target string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		out, err := c.EC2.DescribeNatGateways(&ec2.DescribeNatGatewaysInput{
			NatGatewayIds: []*string{aws.String(id)},
		})
		if err == nil {
			if len(out.NatGateways) == 0 && target == "deleted" {
				return nil
			}
			if len(out.NatGateways) > 0 && aws.StringValue(out.NatGateways[0].State) == target {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("nat gw %s did not reach %s within %s", id, target, timeout)
		}
		time.Sleep(2 * time.Second)
	}
}

// waitForInstanceStateSoft is the cleanup-time analogue of
// harness.WaitForInstanceState — no t.Fatal, just polls and returns.
func waitForInstanceStateSoft(c *harness.AWSClient, id, target string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		out, err := c.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
			InstanceIds: []*string{aws.String(id)},
		})
		if err == nil && len(out.Reservations) > 0 && len(out.Reservations[0].Instances) > 0 {
			if aws.StringValue(out.Reservations[0].Instances[0].State.Name) == target {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("instance %s did not reach %s within %s", id, target, timeout)
		}
		time.Sleep(2 * time.Second)
	}
}

// scpKey copies the harness PEM to /tmp/key.pem on the bastion so it can
// hop into a guest that has no direct SSH ingress of its own.
func scpKey(t *testing.T, keyPath, host string) {
	t.Helper()
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-o", "BatchMode=yes",
		"-i", keyPath,
		keyPath,
		"ubuntu@" + host + ":/tmp/key.pem",
	}
	cmd := exec.Command("scp", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("scp %s -> %s:/tmp/key.pem: %v\n%s", keyPath, host, err, string(out))
	}
}

// sshQuietHardTimeout hard-caps one runSSHQuiet call. ConnectTimeout bounds only
// the TCP connect; a post-connect banner/kex wedge (e.g. probing an EIP whose
// DNAT was just torn down) is otherwise unbounded and can outlast the caller's
// EventuallyErr budget. The ctx deadline kills ssh regardless of which phase hangs.
const sshQuietHardTimeout = 30 * time.Second

// runSSHQuiet is the EventuallyErr-friendly variant of runSSH: it returns
// stdout+stderr and the error rather than calling t.Fatal, so polling
// loops can iterate on transient failures (cloud-init not done, OVN
// flow not installed yet, etc).
func runSSHQuiet(tgt harness.SSHTarget, command string) (string, error) {
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=5",
		"-o", "BatchMode=yes",
		"-p", strconv.Itoa(tgt.Port),
		"-i", tgt.KeyPath,
		tgt.User + "@" + tgt.Host,
		command,
	}
	ctx, cancel := context.WithTimeout(context.Background(), sshQuietHardTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.WaitDelay = 5 * time.Second // return even if ssh ignores the kill
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), fmt.Errorf("ssh %s@%s:%d timed out after %s: %w",
			tgt.User, tgt.Host, tgt.Port, sshQuietHardTimeout, ctx.Err())
	}
	return string(out), err
}
