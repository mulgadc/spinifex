//go:build e2e

package multinode

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// Spread placement + NAT GW (the largest section of run-multinode-e2e.sh,
// originally phase 11) was a single monolithic Test until mulga-siv-127 split
// it into 6 top-level Tests, each owning its own VPC graph via t.Cleanup.
// The 8 bash sub-steps map to the new Tests as:
//
//	bash step 1 (VPC + subnets + IGW + SGs + route)   -> TestMultinodeSpreadVPCSetup
//	bash step 2 (bastion launch + SSH + scp key)      -> TestMultinodeSpreadBastionSSH
//	bash steps 3+4 (spread PG + 3 priv VMs + assert)  -> TestMultinodeSpreadPlacement
//	bash step 5 (ping pre-NAT, expect fail)           -> TestMultinodeSpreadPreNATIsolation
//	bash steps 6+7 (NAT GW + ping post-NAT)           -> TestMultinodeSpreadNATGatewayInternet
//	bash step 8 (explicit teardown ordering)          -> TestMultinodeSpreadNATCleanupOrdering
//
// Tests run sequentially (no t.Parallel) — they all use VPC CIDR 10.100.0.0/16
// and the shared EIP pool, so concurrent runs would collide. Each Test
// re-builds its own VPC + bastion + private trio (the layered ph11SetupX
// helpers below); the cost is intentional — isolation > speed.

// --- Test entry points ----------------------------------------------------

// TestMultinodeSpreadVPCSetup builds the full VPC graph used by the spread
// placement + NAT suite (VPC, public+private subnets, IGW + main RT 0.0.0.0/0
// route, bastion+private SGs) and asserts the IGW route is in place.
func TestMultinodeSpreadVPCSetup(t *testing.T) {
	fix := requireMultiNodeFixture(t)
	harness.Phase(t, "Multinode Spread — VPC Setup")
	v := spreadSetupVPC(t, fix)

	// Assert the 0.0.0.0/0 -> IGW route landed on the main RT. The setup
	// helper requires the CreateRoute call so the test would already fatal
	// on error; the DescribeRouteTables read here is the explicit oracle
	// matching this Test's name.
	out, err := fix.AWS.EC2.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
		RouteTableIds: []*string{aws.String(v.MainRTBID)},
	})
	require.NoError(t, err, "describe main RT")
	require.NotEmpty(t, out.RouteTables, "main RT not found")
	var foundIGW bool
	for _, r := range out.RouteTables[0].Routes {
		if aws.StringValue(r.DestinationCidrBlock) == "0.0.0.0/0" &&
			aws.StringValue(r.GatewayId) == v.IGWID {
			foundIGW = true
			break
		}
	}
	require.Truef(t, foundIGW, "main RT %s missing 0.0.0.0/0 -> %s route", v.MainRTBID, v.IGWID)
}

// TestMultinodeSpreadBastionSSH builds the VPC then launches the bastion and
// waits for an SSH handshake + key SCP. SSH success is the implicit assertion
// (spreadSetupBastion fatals on failure).
func TestMultinodeSpreadBastionSSH(t *testing.T) {
	fix := requireMultiNodeFixture(t)
	harness.Phase(t, "Multinode Spread — Bastion SSH")
	v := spreadSetupVPC(t, fix)
	_ = spreadSetupBastion(t, fix, v)
}

// TestMultinodeSpreadPlacement builds VPC + bastion + spread placement group
// + 3 private VMs and asserts they land on at least 2 distinct hosting nodes
// (>=3 nominal, 2 best-effort per bash).
func TestMultinodeSpreadPlacement(t *testing.T) {
	fix := requireMultiNodeFixture(t)
	harness.Phase(t, "Multinode Spread — Placement Group + 3 Private VMs")
	v := spreadSetupVPC(t, fix)
	b := spreadSetupBastion(t, fix, v)
	p := spreadSetupPrivateTrio(t, fix, b)

	unique := uniqueCount(p.HostingNodes)
	harness.Detail(t, "unique_hosting_nodes", unique, "want_ge", 3)
	switch {
	case unique >= 3:
		// nominal — full spread
	case unique >= 2:
		t.Logf("WARN: only %d unique hosting nodes (expected 3) — spread best-effort", unique)
	default:
		t.Fatalf("spread placement failed: %d unique hosting nodes (want >= 2), hosts=%v", unique, p.HostingNodes)
	}
}

// TestMultinodeSpreadPreNATIsolation builds the full pre-NAT graph and
// asserts ping 8.8.8.8 from each private VM via the bastion fails — i.e.
// no internet without NAT.
func TestMultinodeSpreadPreNATIsolation(t *testing.T) {
	fix := requireMultiNodeFixture(t)
	harness.Phase(t, "Multinode Spread — Pre-NAT Isolation")
	v := spreadSetupVPC(t, fix)
	b := spreadSetupBastion(t, fix, v)
	p := spreadSetupPrivateTrio(t, fix, b)

	for i, privIP := range p.PrivateIPs {
		if _, err := pingViaBastion(b.KeyPath, b.PublicIP, privIP); err == nil {
			t.Fatalf("baseline FAIL: %s (%s) can reach internet WITHOUT NAT GW", p.InstanceIDs[i], privIP)
		}
		harness.Detail(t, "pre_nat", p.InstanceIDs[i], "reachable", false)
	}
}

// TestMultinodeSpreadNATGatewayInternet builds the full graph + NAT Gateway
// and polls ping 8.8.8.8 via each private VM until the OVN SNAT flow lands.
func TestMultinodeSpreadNATGatewayInternet(t *testing.T) {
	fix := requireMultiNodeFixture(t)
	harness.Phase(t, "Multinode Spread — NAT Gateway Internet")
	v := spreadSetupVPC(t, fix)
	b := spreadSetupBastion(t, fix, v)
	p := spreadSetupPrivateTrio(t, fix, b)
	_ = spreadSetupNATGW(t, fix, p)

	harness.Step(t, "verify internet via NAT GW (x%d)", len(p.PrivateIPs))
	for i, privIP := range p.PrivateIPs {
		host := p.HostingNodes[i]
		harness.EventuallyErr(t, func() error {
			if _, err := pingViaBastion(b.KeyPath, b.PublicIP, privIP); err != nil {
				return fmt.Errorf("ping via NAT GW (%s on %s): %w", p.InstanceIDs[i], host, err)
			}
			return nil
		}, 90*time.Second, 5*time.Second)
		harness.Detail(t, "post_nat", p.InstanceIDs[i], "node", host, "reachable", true)
	}
}

// TestMultinodeSpreadNATCleanupOrdering builds the full graph + NAT GW then
// runs the explicit teardown sequence (delete-route, disassociate RT, delete
// RT, delete NAT GW, release EIP) and asserts each call succeeds. The
// deferred t.Cleanup latches flip as each step completes so the LIFO unwind
// is a no-op on the success path.
func TestMultinodeSpreadNATCleanupOrdering(t *testing.T) {
	fix := requireMultiNodeFixture(t)
	harness.Phase(t, "Multinode Spread — Explicit NAT Cleanup Ordering")
	v := spreadSetupVPC(t, fix)
	b := spreadSetupBastion(t, fix, v)
	p := spreadSetupPrivateTrio(t, fix, b)
	n := spreadSetupNATGW(t, fix, p)
	c := fix.AWS

	harness.Step(t, "delete-route 0.0.0.0/0")
	_, err := c.EC2.DeleteRoute(&ec2.DeleteRouteInput{
		RouteTableId:         aws.String(n.PrivRTBID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
	})
	require.NoError(t, err, "delete-route 0.0.0.0/0")
	*n.RouteDeleted = true

	harness.Step(t, "disassociate-route-table %s", n.AssocID)
	_, err = c.EC2.DisassociateRouteTable(&ec2.DisassociateRouteTableInput{
		AssociationId: aws.String(n.AssocID),
	})
	require.NoError(t, err, "disassociate private RT")
	*n.AssocReleased = true

	harness.Step(t, "delete-route-table %s", n.PrivRTBID)
	_, err = c.EC2.DeleteRouteTable(&ec2.DeleteRouteTableInput{
		RouteTableId: aws.String(n.PrivRTBID),
	})
	require.NoError(t, err, "delete private RT")
	*n.RTBDeleted = true

	harness.Step(t, "delete-nat-gateway %s", n.NatGWID)
	_, err = c.EC2.DeleteNatGateway(&ec2.DeleteNatGatewayInput{
		NatGatewayId: aws.String(n.NatGWID),
	})
	require.NoError(t, err, "delete-nat-gateway")
	waitForNATGatewayStateFatal(t, c, n.NatGWID, "deleted")
	*n.NatDeleted = true

	harness.Step(t, "release-address %s", n.EIPAllocID)
	_, err = c.EC2.ReleaseAddress(&ec2.ReleaseAddressInput{
		AllocationId: aws.String(n.EIPAllocID),
	})
	require.NoError(t, err, "release-address")
	*n.EIPReleased = true
}

// --- Layered setup helpers ------------------------------------------------

// spreadVPC carries the IDs produced by spreadSetupVPC. Public + private subnet,
// IGW + main RT (with 0.0.0.0/0 IGW route), and bastion+private SGs.
type spreadVPC struct {
	VPCID        string
	PubSubnetID  string
	PrivSubnetID string
	IGWID        string
	BastionSGID  string
	PrivSGID     string
	MainRTBID    string
}

// spreadSetupVPC creates the phase-11 VPC graph (VPC + subnets + IGW + main
// RT route + SGs) and registers LIFO cleanup against t. Fatals on any AWS
// error. Mirrors run-multinode-e2e.sh phase 11 step 1.
func spreadSetupVPC(t *testing.T, fix *Fixture) spreadVPC {
	t.Helper()
	c := fix.AWS

	harness.Step(t, "create VPC 10.100.0.0/16 + subnets + IGW + SGs")
	vpcOut, err := c.EC2.CreateVpc(&ec2.CreateVpcInput{
		CidrBlock: aws.String("10.100.0.0/16"),
	})
	require.NoError(t, err, "create-vpc 10.100.0.0/16")
	vpcID := aws.StringValue(vpcOut.Vpc.VpcId)
	require.NotEmpty(t, vpcID, "VpcId empty")
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(vpcID)})
	})

	pubSubOut, err := c.EC2.CreateSubnet(&ec2.CreateSubnetInput{
		VpcId:     aws.String(vpcID),
		CidrBlock: aws.String("10.100.1.0/24"),
	})
	require.NoError(t, err, "create public subnet")
	pubSubnetID := aws.StringValue(pubSubOut.Subnet.SubnetId)
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String(pubSubnetID)})
	})

	privSubOut, err := c.EC2.CreateSubnet(&ec2.CreateSubnetInput{
		VpcId:     aws.String(vpcID),
		CidrBlock: aws.String("10.100.2.0/24"),
	})
	require.NoError(t, err, "create private subnet")
	privSubnetID := aws.StringValue(privSubOut.Subnet.SubnetId)
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String(privSubnetID)})
	})

	_, err = c.EC2.ModifySubnetAttribute(&ec2.ModifySubnetAttributeInput{
		SubnetId:            aws.String(pubSubnetID),
		MapPublicIpOnLaunch: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	})
	require.NoError(t, err, "modify-subnet-attribute MapPublicIpOnLaunch")

	igwOut, err := c.EC2.CreateInternetGateway(&ec2.CreateInternetGatewayInput{})
	require.NoError(t, err, "create-internet-gateway")
	igwID := aws.StringValue(igwOut.InternetGateway.InternetGatewayId)
	t.Cleanup(func() {
		_, _ = c.EC2.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
			InternetGatewayId: aws.String(igwID),
			VpcId:             aws.String(vpcID),
		})
		_, _ = c.EC2.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{
			InternetGatewayId: aws.String(igwID),
		})
	})
	_, err = c.EC2.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String(vpcID),
	})
	require.NoError(t, err, "attach-internet-gateway")

	mainRT, err := c.EC2.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("vpc-id"), Values: []*string{aws.String(vpcID)}},
		},
	})
	require.NoError(t, err, "describe-route-tables (main)")
	require.NotEmpty(t, mainRT.RouteTables, "no main route table for fresh VPC")
	mainRTBID := aws.StringValue(mainRT.RouteTables[0].RouteTableId)
	_, err = c.EC2.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(mainRTBID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String(igwID),
	})
	require.NoError(t, err, "create-route main RT -> IGW")

	bastionSGOut, err := c.EC2.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		VpcId:       aws.String(vpcID),
		GroupName:   aws.String("natgw-bastion"),
		Description: aws.String("Spread bastion (SSH ingress from anywhere)"),
	})
	require.NoError(t, err, "create bastion SG")
	bastionSGID := aws.StringValue(bastionSGOut.GroupId)
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{
			GroupId: aws.String(bastionSGID),
		})
	})
	_, err = c.EC2.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(bastionSGID),
		IpPermissions: []*ec2.IpPermission{
			{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int64(22),
				ToPort:     aws.Int64(22),
				IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("0.0.0.0/0")}},
			},
		},
	})
	require.NoError(t, err, "authorize bastion SG tcp/22")

	privSGOut, err := c.EC2.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		VpcId:       aws.String(vpcID),
		GroupName:   aws.String("natgw-private"),
		Description: aws.String("Spread private (SSH from bastion-sg, ICMP from VPC)"),
	})
	require.NoError(t, err, "create private SG")
	privSGID := aws.StringValue(privSGOut.GroupId)
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{
			GroupId: aws.String(privSGID),
		})
	})
	_, err = c.EC2.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(privSGID),
		IpPermissions: []*ec2.IpPermission{
			{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int64(22),
				ToPort:     aws.Int64(22),
				UserIdGroupPairs: []*ec2.UserIdGroupPair{
					{GroupId: aws.String(bastionSGID)},
				},
			},
		},
	})
	require.NoError(t, err, "authorize private SG tcp/22 from bastion-sg")
	_, err = c.EC2.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(privSGID),
		IpPermissions: []*ec2.IpPermission{
			{
				IpProtocol: aws.String("icmp"),
				FromPort:   aws.Int64(-1),
				ToPort:     aws.Int64(-1),
				IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("10.100.0.0/16")}},
			},
		},
	})
	require.NoError(t, err, "authorize private SG icmp from VPC")

	harness.Detail(t, "vpc", vpcID, "pub_subnet", pubSubnetID, "priv_subnet", privSubnetID,
		"igw", igwID, "bastion_sg", bastionSGID, "priv_sg", privSGID)

	return spreadVPC{
		VPCID:        vpcID,
		PubSubnetID:  pubSubnetID,
		PrivSubnetID: privSubnetID,
		IGWID:        igwID,
		BastionSGID:  bastionSGID,
		PrivSGID:     privSGID,
		MainRTBID:    mainRTBID,
	}
}

// spreadBastion carries the bastion VM IDs plus the VPC it lives in.
type spreadBastion struct {
	VPC      spreadVPC
	ID       string
	PublicIP string
	KeyName  string
	KeyPath  string
}

// spreadSetupBastion launches the bastion in the public subnet, waits for SSH,
// and copies the test PEM to /tmp/key.pem so the bastion can re-key into
// private VMs. Registers terminate + diagnostic-on-failure cleanup. Mirrors
// run-multinode-e2e.sh phase 11 step 2.
func spreadSetupBastion(t *testing.T, fix *Fixture, v spreadVPC) spreadBastion {
	t.Helper()
	c := fix.AWS

	amiID := needAMI(t, fix, needArch(t, fix))
	instType, _ := needInstanceTypeArch(t, fix)
	keyName, keyPath := needKeyPair(t, fix)

	harness.Step(t, "launch bastion in public subnet")
	bastionOut, err := c.EC2.RunInstances(&ec2.RunInstancesInput{
		ImageId:          aws.String(amiID),
		InstanceType:     aws.String(instType),
		KeyName:          aws.String(keyName),
		SubnetId:         aws.String(v.PubSubnetID),
		SecurityGroupIds: []*string{aws.String(v.BastionSGID)},
		MinCount:         aws.Int64(1),
		MaxCount:         aws.Int64(1),
	})
	require.NoError(t, err, "run-instances bastion")
	require.NotEmpty(t, bastionOut.Instances, "run-instances bastion returned no Instances")
	bastionID := aws.StringValue(bastionOut.Instances[0].InstanceId)
	require.NotEmpty(t, bastionID, "bastion InstanceId empty")
	t.Cleanup(func() {
		_, _ = c.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
			InstanceIds: []*string{aws.String(bastionID)},
		})
		_ = waitForInstanceStateBest(c, bastionID, "terminated", 5*time.Minute)
	})

	bastion := harness.WaitForInstanceState(t, c, bastionID, "running",
		harness.WithTimeout(120*time.Second), harness.WithPoll(2*time.Second))
	bastionPubIP := aws.StringValue(bastion.PublicIpAddress)
	require.NotEmptyf(t, bastionPubIP,
		"bastion %s has no PublicIpAddress (pool mode required for phase 11)", bastionID)
	harness.Detail(t, "bastion", bastionID, "bastion_pub_ip", bastionPubIP)

	// veth bridge mode (run 26198221557 journal) routes EIP ingress through
	// the priority-20 gateway-chassis; with a 3-node cluster the gw chassis
	// is the same node as the bastion only ~33% of the time, so ARP +
	// geneve traversal need observing to localise SYN drops on failure.
	t.Cleanup(func() {
		if t.Failed() {
			dumpPlacementNATDiag(t, fix, bastionPubIP, bastionID)
		}
	})

	waitForBastionSSH(t, bastionPubIP, keyPath, 5*time.Minute)
	scpKeyToBastion(t, keyPath, bastionPubIP)

	return spreadBastion{
		VPC:      v,
		ID:       bastionID,
		PublicIP: bastionPubIP,
		KeyName:  keyName,
		KeyPath:  keyPath,
	}
}

// spreadPrivate carries the spread-placement trio: PG name, 3 private instance
// IDs, their private IPs, and the cluster node each one landed on.
type spreadPrivate struct {
	Bastion      spreadBastion
	InstanceIDs  []string
	PrivateIPs   []string
	HostingNodes []string
	PGName       string
}

// spreadSetupPrivateTrio creates the spread placement group + 3 private VMs,
// waits for SSH-via-bastion, and resolves each instance's hosting node and
// private IP. Mirrors run-multinode-e2e.sh phase 11 step 3.
func spreadSetupPrivateTrio(t *testing.T, fix *Fixture, b spreadBastion) spreadPrivate {
	t.Helper()
	c := fix.AWS

	amiID := needAMI(t, fix, needArch(t, fix))
	instType, _ := needInstanceTypeArch(t, fix)

	harness.Step(t, "create spread placement group + 3 private VMs")
	pgName := "nat-spread"
	_, err := c.EC2.CreatePlacementGroup(&ec2.CreatePlacementGroupInput{
		GroupName: aws.String(pgName),
		Strategy:  aws.String("spread"),
	})
	require.NoError(t, err, "create-placement-group nat-spread")
	t.Cleanup(func() {
		_, _ = c.EC2.DeletePlacementGroup(&ec2.DeletePlacementGroupInput{
			GroupName: aws.String(pgName),
		})
	})

	privOut, err := c.EC2.RunInstances(&ec2.RunInstancesInput{
		ImageId:          aws.String(amiID),
		InstanceType:     aws.String(instType),
		KeyName:          aws.String(b.KeyName),
		SubnetId:         aws.String(b.VPC.PrivSubnetID),
		SecurityGroupIds: []*string{aws.String(b.VPC.PrivSGID)},
		MinCount:         aws.Int64(3),
		MaxCount:         aws.Int64(3),
		Placement: &ec2.Placement{
			GroupName: aws.String(pgName),
		},
	})
	require.NoError(t, err, "run-instances private x3")
	require.Lenf(t, privOut.Instances, 3, "run-instances private returned %d instances", len(privOut.Instances))

	privIDs := make([]string, 0, 3)
	for _, inst := range privOut.Instances {
		id := aws.StringValue(inst.InstanceId)
		require.NotEmpty(t, id, "private InstanceId empty")
		privIDs = append(privIDs, id)
		idCopy := id
		t.Cleanup(func() {
			_, _ = c.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
				InstanceIds: []*string{aws.String(idCopy)},
			})
			_ = waitForInstanceStateBest(c, idCopy, "terminated", 5*time.Minute)
		})
	}
	harness.Detail(t, "private_ids", privIDs)

	for _, id := range privIDs {
		harness.WaitForInstanceState(t, c, id, "running",
			harness.WithTimeout(120*time.Second), harness.WithPoll(2*time.Second))
	}

	hostingNodes := make([]string, 0, len(privIDs))
	for _, id := range privIDs {
		n := harness.InstanceHostingNode(t, fix.Cluster, id)
		if n == nil {
			hostingNodes = append(hostingNodes, "unknown")
			harness.Detail(t, "instance", id, "host", "unknown")
			continue
		}
		hostingNodes = append(hostingNodes, n.Name)
		harness.Detail(t, "instance", id, "host", n.Name)
	}

	privIPs := make([]string, 0, len(privIDs))
	for _, id := range privIDs {
		out, err := c.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
			InstanceIds: []*string{aws.String(id)},
		})
		require.NoError(t, err, "describe-instances private")
		require.NotEmpty(t, out.Reservations, "no Reservations for %s", id)
		require.NotEmpty(t, out.Reservations[0].Instances, "no Instances for %s", id)
		ip := aws.StringValue(out.Reservations[0].Instances[0].PrivateIpAddress)
		require.NotEmptyf(t, ip, "%s has no PrivateIpAddress", id)
		privIPs = append(privIPs, ip)
	}
	harness.Detail(t, "private_ips", privIPs)

	harness.Step(t, "wait for private SSH via bastion (x%d)", len(privIPs))
	for i, privIP := range privIPs {
		waitForSSHViaBastion(t, b.KeyPath, b.PublicIP, privIP, fmt.Sprintf("priv #%d (%s)", i, privIDs[i]))
	}

	return spreadPrivate{
		Bastion:      b,
		InstanceIDs:  privIDs,
		PrivateIPs:   privIPs,
		HostingNodes: hostingNodes,
		PGName:       pgName,
	}
}

// spreadNAT carries the NAT GW + private RT IDs. The *bool fields are the
// "already torn down" latches used by TestMultinodePhase11Cleanup to flip
// the deferred cleanup closures into no-ops as each explicit teardown step
// succeeds.
type spreadNAT struct {
	Private       spreadPrivate
	NatGWID       string
	EIPAllocID    string
	EIP           string
	PrivRTBID     string
	AssocID       string
	NatDeleted    *bool
	EIPReleased   *bool
	RTBDeleted    *bool
	AssocReleased *bool
	RouteDeleted  *bool
}

// spreadSetupNATGW allocates an EIP, creates the NAT GW in the public subnet,
// builds the private RT + association + 0.0.0.0/0 -> NAT GW route, and waits
// for the NAT GW to reach `available`. Mirrors run-multinode-e2e.sh phase 11
// step 6.
func spreadSetupNATGW(t *testing.T, fix *Fixture, p spreadPrivate) spreadNAT {
	t.Helper()
	c := fix.AWS

	harness.Step(t, "allocate EIP + create NAT GW + private RT")
	eipOut, err := c.EC2.AllocateAddress(&ec2.AllocateAddressInput{
		Domain: aws.String("vpc"),
	})
	require.NoError(t, err, "allocate-address vpc")
	natAllocID := aws.StringValue(eipOut.AllocationId)
	natPubIP := aws.StringValue(eipOut.PublicIp)
	require.NotEmpty(t, natAllocID, "EIP AllocationId empty")
	eipReleased := new(bool)
	t.Cleanup(func() {
		if !*eipReleased {
			_, _ = c.EC2.ReleaseAddress(&ec2.ReleaseAddressInput{
				AllocationId: aws.String(natAllocID),
			})
		}
	})
	harness.Detail(t, "eip", natPubIP, "alloc", natAllocID)

	natOut, err := c.EC2.CreateNatGateway(&ec2.CreateNatGatewayInput{
		SubnetId:     aws.String(p.Bastion.VPC.PubSubnetID),
		AllocationId: aws.String(natAllocID),
	})
	require.NoError(t, err, "create-nat-gateway")
	natGWID := aws.StringValue(natOut.NatGateway.NatGatewayId)
	require.NotEmpty(t, natGWID, "NatGatewayId empty")
	natDeleted := new(bool)
	t.Cleanup(func() {
		if !*natDeleted {
			_, _ = c.EC2.DeleteNatGateway(&ec2.DeleteNatGatewayInput{
				NatGatewayId: aws.String(natGWID),
			})
			_ = waitForNATGatewayStateBest(c, natGWID, "deleted", 5*time.Minute)
		}
	})
	harness.Detail(t, "nat_gw", natGWID)
	waitForNATGatewayStateFatal(t, c, natGWID, "available")

	privRTOut, err := c.EC2.CreateRouteTable(&ec2.CreateRouteTableInput{
		VpcId: aws.String(p.Bastion.VPC.VPCID),
	})
	require.NoError(t, err, "create-route-table (private)")
	privRTBID := aws.StringValue(privRTOut.RouteTable.RouteTableId)
	require.NotEmpty(t, privRTBID, "private RouteTableId empty")
	rtbDeleted := new(bool)
	t.Cleanup(func() {
		if !*rtbDeleted {
			_, _ = c.EC2.DeleteRouteTable(&ec2.DeleteRouteTableInput{
				RouteTableId: aws.String(privRTBID),
			})
		}
	})

	rtbAssocOut, err := c.EC2.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(privRTBID),
		SubnetId:     aws.String(p.Bastion.VPC.PrivSubnetID),
	})
	require.NoError(t, err, "associate private RT")
	rtbAssocID := aws.StringValue(rtbAssocOut.AssociationId)
	assocReleased := new(bool)
	t.Cleanup(func() {
		if !*assocReleased {
			_, _ = c.EC2.DisassociateRouteTable(&ec2.DisassociateRouteTableInput{
				AssociationId: aws.String(rtbAssocID),
			})
		}
	})

	_, err = c.EC2.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(privRTBID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		NatGatewayId:         aws.String(natGWID),
	})
	require.NoError(t, err, "create-route 0.0.0.0/0 -> NAT GW")
	routeDeleted := new(bool)
	t.Cleanup(func() {
		if !*routeDeleted {
			_, _ = c.EC2.DeleteRoute(&ec2.DeleteRouteInput{
				RouteTableId:         aws.String(privRTBID),
				DestinationCidrBlock: aws.String("0.0.0.0/0"),
			})
		}
	})

	return spreadNAT{
		Private:       p,
		NatGWID:       natGWID,
		EIPAllocID:    natAllocID,
		EIP:           natPubIP,
		PrivRTBID:     privRTBID,
		AssocID:       rtbAssocID,
		NatDeleted:    natDeleted,
		EIPReleased:   eipReleased,
		RTBDeleted:    rtbDeleted,
		AssocReleased: assocReleased,
		RouteDeleted:  routeDeleted,
	}
}

// --- helpers --------------------------------------------------------------

// uniqueCount returns the number of distinct non-empty / non-"unknown"
// strings in xs. Matches the bash `sort -u | wc -l` line that gates the
// spread-placement pass.
func uniqueCount(xs []string) int {
	seen := map[string]bool{}
	for _, x := range xs {
		if x == "" || x == "unknown" {
			continue
		}
		seen[x] = true
	}
	return len(seen)
}

// waitForBastionSSH polls SSH `true` until the handshake completes. Bash
// equivalent: phase 11 Step 2's 30-attempt 5s loop = 150s budget. Go port
// uses a wider 5min budget because a brand-new VPC's OVN flow install on
// the external switch lags the instance's "running" state on the tofu
// cluster (mulga-siv-90 runs 26195522455, 26197248656, 26198221557 — SSH
// timed out at exactly 150s even after the dnat_and_snat race fix
// mulga-siv-124 landed). 5min covers both cloud-init + OVN warm-up.
//
// Logs a single ICMP probe per iteration before the SSH dial so a future
// failure log distinguishes "datapath down" (ping fails) from "sshd not
// up yet" (ping ok, ssh refused/timeout) — the smoking-gun split per
// the RCA agent's fix candidate 3.
func waitForBastionSSH(t *testing.T, host, keyPath string, timeout time.Duration) {
	t.Helper()
	harness.Step(t, "wait bastion SSH %s", host)
	harness.EventuallyErr(t, func() error {
		pingCtx, pingCancel := context.WithTimeout(context.Background(), 3*time.Second)
		pingOut, pingErr := exec.CommandContext(pingCtx, "ping", "-c", "1", "-W", "2", host).CombinedOutput()
		pingCancel()
		pingState := "ok"
		if pingErr != nil {
			pingState = fmt.Sprintf("fail (%v)", pingErr)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		args := []string{
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-o", "LogLevel=ERROR",
			"-o", "ConnectTimeout=5",
			"-o", "BatchMode=yes",
			"-i", keyPath,
			"ec2-user@" + host,
			"true",
		}
		if out, err := exec.CommandContext(ctx, "ssh", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("ssh %s: %w\n  ping=%s\n  ping_output=%s\n  ssh_output=%s",
				host, err, pingState, strings.TrimSpace(string(pingOut)), string(out))
		}
		return nil
	}, timeout, 5*time.Second)
}

// scpKeyToBastion copies the test PEM to /tmp/key.pem on the bastion and
// chmods it so the bastion can re-key into private VMs.
func scpKeyToBastion(t *testing.T, keyPath, host string) {
	t.Helper()
	harness.Step(t, "scp key -> bastion:/tmp/key.pem")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	scpArgs := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-o", "BatchMode=yes",
		"-i", keyPath,
		keyPath,
		"ec2-user@" + host + ":/tmp/key.pem",
	}
	if out, err := exec.CommandContext(ctx, "scp", scpArgs...).CombinedOutput(); err != nil {
		t.Fatalf("scp %s -> %s:/tmp/key.pem: %v\n%s", keyPath, host, err, string(out))
	}
	// chmod 600 — sshd refuses keys with loose perms.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
	sshArgs := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-o", "BatchMode=yes",
		"-i", keyPath,
		"ec2-user@" + host,
		"chmod 600 /tmp/key.pem",
	}
	if out, err := exec.CommandContext(ctx2, "ssh", sshArgs...).CombinedOutput(); err != nil {
		t.Fatalf("chmod /tmp/key.pem: %v\n%s", err, string(out))
	}
}

// sshViaBastion runs cmd on privIP through bastionHost using BatchMode +
// ConnectTimeout=10 nested-ssh — mirrors the bash priv_hop_cmd helper.
// Returns combined stdout+stderr and the exit error.
func sshViaBastion(keyPath, bastionHost, privIP, cmd string) (string, error) {
	hop := fmt.Sprintf(
		"ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null "+
			"-o LogLevel=ERROR -o ConnectTimeout=10 -o BatchMode=yes "+
			"-i /tmp/key.pem ec2-user@%s '%s'",
		privIP, cmd,
	)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-o", "BatchMode=yes",
		"-i", keyPath,
		"ec2-user@" + bastionHost,
		hop,
	}
	out, err := exec.CommandContext(ctx, "ssh", args...).CombinedOutput()
	return string(out), err
}

// waitForSSHViaBastion polls hostname-through-bastion until cloud-init on
// the private VM has finished bringing sshd up. label is a triage tag for
// the log line; bash uses a similar attempt loop with 30 tries x 5s.
func waitForSSHViaBastion(t *testing.T, keyPath, bastionHost, privIP, label string) {
	t.Helper()
	harness.EventuallyErr(t, func() error {
		out, err := sshViaBastion(keyPath, bastionHost, privIP, "hostname")
		if err != nil {
			return fmt.Errorf("%s: bastion->%s hostname: %w\n%s", label, privIP, err, out)
		}
		if strings.TrimSpace(out) == "" {
			return fmt.Errorf("%s: empty hostname response", label)
		}
		return nil
	}, 3*time.Minute, 5*time.Second)
}

// pingViaBastion sends a single 8.8.8.8 ping through the bastion hop. Used
// twice: pre-NAT (expect err) and post-NAT (expect nil after a poll).
func pingViaBastion(keyPath, bastionHost, privIP string) (string, error) {
	return sshViaBastion(keyPath, bastionHost, privIP, "ping -c 1 -W 3 8.8.8.8")
}

// --- NAT GW state pollers (multinode-local; phase 8d single-node has its
// own copies — promote to harness/poll.go once a third consumer lands).

func waitForNATGatewayStateFatal(t *testing.T, c *harness.AWSClient, id, target string) {
	t.Helper()
	harness.EventuallyErr(t, func() error {
		out, err := c.EC2.DescribeNatGateways(&ec2.DescribeNatGatewaysInput{
			NatGatewayIds: []*string{aws.String(id)},
		})
		if err != nil {
			return fmt.Errorf("describe-nat-gateways %s: %w", id, err)
		}
		if len(out.NatGateways) == 0 {
			if target == "deleted" {
				return nil
			}
			return fmt.Errorf("%s not found", id)
		}
		state := aws.StringValue(out.NatGateways[0].State)
		if state == target {
			return nil
		}
		if state == "failed" {
			msg := aws.StringValue(out.NatGateways[0].FailureMessage)
			return fmt.Errorf("%s entered failed state: %s", id, msg)
		}
		return fmt.Errorf("%s state=%s want=%s", id, state, target)
	}, 5*time.Minute, 2*time.Second)
}

func waitForNATGatewayStateBest(c *harness.AWSClient, id, target string, timeout time.Duration) error {
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

// dumpPlacementNATDiag fans out a fixed shortlist of datapath probes to
// every cluster node and writes results to <Artifacts>/diag-<node>-<probe>.log.
// Triggered only on test failure (via t.Cleanup wrapper) so a green run
// leaves no extra files. Best-effort: per-node SSH errors are logged but
// never re-fail the test.
//
// Probe set targets the centralized-NAT EIP datapath:
//   - arp -an              : did ARP for the EIP resolve on this node?
//   - ip route get <eip>   : kernel routing decision for outbound to EIP
//   - ovs-vsctl show       : bridges + ports (br-int, br-ex)
//   - ovs-vsctl get ... ovn-bridge-mappings : physnet binding
//   - ovn-sbctl list chassis              : encap IPs + gateway hostnames
//   - ovn-nbctl list nat                  : dnat_and_snat rule state
//   - ovn-nbctl list gateway_chassis      : priority election state
//   - ovn-nbctl find logical_router_port  : DGP gateway chassis attachment
func dumpPlacementNATDiag(t *testing.T, fix *Fixture, bastionPubIP, bastionID string) {
	t.Helper()
	if fix.Artifacts == "" {
		t.Logf("dumpPlacementNATDiag: no artifact dir; skipping")
		return
	}
	if fix.Cluster == nil || len(fix.Cluster.Nodes) == 0 {
		t.Logf("dumpPlacementNATDiag: no cluster nodes; skipping")
		return
	}

	t.Logf("dumpPlacementNATDiag: bastion=%s eip=%s nodes=%d -> %s",
		bastionID, bastionPubIP, len(fix.Cluster.Nodes), fix.Artifacts)

	probes := []struct {
		name string
		cmd  string
	}{
		{"arp", "arp -an"},
		{"ip-route-eip", fmt.Sprintf("ip route get %s 2>&1 || true", bastionPubIP)},
		{"ip-addr", "ip -br addr"},
		{"ovs-show", "sudo ovs-vsctl show"},
		{"ovs-bridge-mappings", "sudo ovs-vsctl get Open_vSwitch . external_ids:ovn-bridge-mappings 2>&1 || true"},
		{"ovn-sb-chassis", "sudo ovn-sbctl --no-leader-only list chassis 2>&1 || true"},
		{"ovn-nb-nat", "sudo ovn-nbctl --no-leader-only list nat 2>&1 || true"},
		{"ovn-nb-gw-chassis", "sudo ovn-nbctl --no-leader-only list gateway_chassis 2>&1 || true"},
		{"ovn-nb-lrp", "sudo ovn-nbctl --no-leader-only list logical_router_port 2>&1 || true"},
		{"ip-link-geneve", "ip -d link show type geneve 2>&1 || true"},
	}

	ssh := harness.NewPeerSSH()
	for _, n := range fix.Cluster.Nodes {
		for _, p := range probes {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			out, err := ssh.Run(ctx, n.Addr, p.cmd)
			cancel()
			header := fmt.Sprintf("# host=%s cmd=%s\n", n.Addr, p.cmd)
			if err != nil {
				header += fmt.Sprintf("# (ssh error: %v)\n", err)
			}
			name := fmt.Sprintf("diag-%s-%s.log", n.Name, p.name)
			harness.DumpFile(t, fix.Artifacts, name, append([]byte(header), out...))
		}
	}

	localProbes := []struct {
		name string
		cmd  string
		args []string
	}{
		{"local-ping-eip", "ping", []string{"-c", "3", "-W", "2", bastionPubIP}},
		{"local-traceroute-eip", "traceroute", []string{"-n", "-w", "2", "-m", "12", bastionPubIP}},
		{"local-ip-route-eip", "ip", []string{"route", "get", bastionPubIP}},
		{"local-arp", "arp", []string{"-an"}},
	}
	for _, p := range localProbes {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		out, err := exec.CommandContext(ctx, p.cmd, p.args...).CombinedOutput()
		cancel()
		header := fmt.Sprintf("$ %s %s\n", p.cmd, strings.Join(p.args, " "))
		if err != nil {
			header += fmt.Sprintf("(exit error: %v)\n", err)
		}
		harness.DumpFile(t, fix.Artifacts, p.name+".log", append([]byte(header), out...))
	}
}

// waitForInstanceStateBest is the cleanup-time analogue of
// harness.WaitForInstanceState — no t.Fatal, just polls + returns.
func waitForInstanceStateBest(c *harness.AWSClient, id, target string, timeout time.Duration) error {
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
