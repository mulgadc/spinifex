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

// phase11_PlacementAndNATGateway is the Go port of phase 11 from
// run-multinode-e2e.sh:1255-1594. The largest single phase — bash spans
// 8 sub-steps. The Go port keeps the same sub-step boundaries (Step1..Step8)
// in harness.Step lines so a triager can cross-reference logs.
//
// Sub-steps:
//
//	Step 1: VPC + public/private subnets + IGW + main RT route + SGs
//	Step 2: Bastion in public subnet, wait SSH, scp key
//	Step 3: Spread placement group + 3 private instances
//	Step 4: Validate spread across nodes (>=2 unique nodes; >=3 = PASS)
//	Step 5: Verify private VMs have NO internet pre-NAT
//	Step 6: Create NAT GW (EIP + create-nat-gateway + priv RT + route)
//	Step 7: Verify internet via NAT GW from all 3 private VMs
//	Step 8: Cleanup NAT GW resources inline
//
// Every resource is owned by the test — no Ensure* memoization — so a
// failure mid-flight tears the full VPC graph down via t.Cleanup (LIFO).
// Bash mirrors the same teardown order.
func phase11_PlacementAndNATGateway(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode Phase 11 — Spread Placement + NAT Gateway")

	require.GreaterOrEqualf(t, len(fix.Cluster.Nodes), 3,
		"phase 11 requires a 3-node cluster, have %d", len(fix.Cluster.Nodes))

	amiID := needAMI(t, fix, needArch(t, fix))
	instType, _ := needInstanceTypeArch(t, fix)
	keyName, keyPath := needKeyPair(t, fix)
	c := fix.AWS

	// --- Step 1: VPC infrastructure ---------------------------------------
	harness.Step(t, "Step 1 — create VPC + subnets + IGW + SGs")

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

	// Public subnet MapPublicIpOnLaunch — bastion picks up routable IP.
	_, err = c.EC2.ModifySubnetAttribute(&ec2.ModifySubnetAttributeInput{
		SubnetId:            aws.String(pubSubnetID),
		MapPublicIpOnLaunch: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	})
	require.NoError(t, err, "modify-subnet-attribute MapPublicIpOnLaunch")

	igwOut, err := c.EC2.CreateInternetGateway(&ec2.CreateInternetGatewayInput{})
	require.NoError(t, err, "create-internet-gateway")
	igwID := aws.StringValue(igwOut.InternetGateway.InternetGatewayId)
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
	_, err = c.EC2.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String(vpcID),
	})
	require.NoError(t, err, "attach-internet-gateway")

	// Find main route table for the VPC (auto-created by CreateVpc) and
	// add a 0.0.0.0/0 -> IGW route so the public subnet has egress.
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

	// SGs: bastion (tcp/22 from anywhere) + private (tcp/22 from bastion-sg,
	// icmp from VPC).
	bastionSGOut, err := c.EC2.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		VpcId:       aws.String(vpcID),
		GroupName:   aws.String("natgw-bastion"),
		Description: aws.String("Phase 11 bastion (SSH ingress from anywhere)"),
	})
	require.NoError(t, err, "create bastion SG")
	bastionSGID := aws.StringValue(bastionSGOut.GroupId)
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{
			GroupId: aws.String(bastionSGID),
		})
	})
	_, err = c.EC2.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId:    aws.String(bastionSGID),
		IpProtocol: aws.String("tcp"),
		FromPort:   aws.Int64(22),
		ToPort:     aws.Int64(22),
		CidrIp:     aws.String("0.0.0.0/0"),
	})
	require.NoError(t, err, "authorize bastion SG tcp/22")

	privSGOut, err := c.EC2.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		VpcId:       aws.String(vpcID),
		GroupName:   aws.String("natgw-private"),
		Description: aws.String("Phase 11 private (SSH from bastion-sg, ICMP from VPC)"),
	})
	require.NoError(t, err, "create private SG")
	privSGID := aws.StringValue(privSGOut.GroupId)
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{
			GroupId: aws.String(privSGID),
		})
	})
	// tcp/22 from bastion-sg via UserIdGroupPair
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
	// icmp from VPC CIDR
	_, err = c.EC2.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId:    aws.String(privSGID),
		IpProtocol: aws.String("icmp"),
		FromPort:   aws.Int64(-1),
		ToPort:     aws.Int64(-1),
		CidrIp:     aws.String("10.100.0.0/16"),
	})
	require.NoError(t, err, "authorize private SG icmp from VPC")
	harness.Detail(t, "vpc", vpcID, "pub_subnet", pubSubnetID, "priv_subnet", privSubnetID,
		"igw", igwID, "bastion_sg", bastionSGID, "priv_sg", privSGID)

	// --- Step 2: Bastion ---------------------------------------------------
	harness.Step(t, "Step 2 — launch bastion in public subnet")
	bastionOut, err := c.EC2.RunInstances(&ec2.RunInstancesInput{
		ImageId:          aws.String(amiID),
		InstanceType:     aws.String(instType),
		KeyName:          aws.String(keyName),
		SubnetId:         aws.String(pubSubnetID),
		SecurityGroupIds: []*string{aws.String(bastionSGID)},
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

	waitForBastionSSH(t, bastionPubIP, keyPath, 150*time.Second)
	scpKeyToBastion(t, keyPath, bastionPubIP)

	// --- Step 3: Placement group + 3 private VMs --------------------------
	harness.Step(t, "Step 3 — create spread placement group + 3 private VMs")
	pgName := "nat-spread"
	_, err = c.EC2.CreatePlacementGroup(&ec2.CreatePlacementGroupInput{
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
		KeyName:          aws.String(keyName),
		SubnetId:         aws.String(privSubnetID),
		SecurityGroupIds: []*string{aws.String(privSGID)},
		MinCount:         aws.Int64(3),
		MaxCount:         aws.Int64(3),
		Placement: &ec2.Placement{
			GroupName: aws.String(pgName),
		},
	})
	require.NoError(t, err, "run-instances private x3")
	require.Lenf(t, privOut.Instances, 3, "run-instances private returned %d instances", len(privOut.Instances))
	var privIDs []string
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

	// --- Step 4: Validate spread across nodes -----------------------------
	harness.Step(t, "Step 4 — validate spread placement across nodes")
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
	unique := uniqueCount(hostingNodes)
	harness.Detail(t, "unique_hosting_nodes", unique, "want_ge", 3)
	if unique >= 3 {
		// nominal — full spread
	} else if unique >= 2 {
		t.Logf("WARN: only %d unique hosting nodes (expected 3) — spread best-effort", unique)
	} else {
		t.Fatalf("spread placement failed: %d unique hosting nodes (want >= 2), hosts=%v", unique, hostingNodes)
	}

	// Collect private IPs for the bastion-hop probes.
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

	// Wait for SSH via bastion to every private VM. Cloud-init can drag
	// 30-40s past "running".
	harness.Step(t, "wait for private SSH via bastion (x%d)", len(privIPs))
	for i, privIP := range privIPs {
		waitForSSHViaBastion(t, keyPath, bastionPubIP, privIP, fmt.Sprintf("priv #%d (%s)", i, privIDs[i]))
	}

	// --- Step 5: Verify NO internet pre-NAT --------------------------------
	harness.Step(t, "Step 5 — verify private VMs have NO internet pre-NAT")
	for i, privIP := range privIPs {
		if _, err := pingViaBastion(keyPath, bastionPubIP, privIP); err == nil {
			t.Fatalf("baseline FAIL: %s (%s) can reach internet WITHOUT NAT GW", privIDs[i], privIP)
		}
		harness.Detail(t, "pre_nat", privIDs[i], "reachable", false)
	}

	// --- Step 6: Create NAT Gateway ----------------------------------------
	harness.Step(t, "Step 6 — allocate EIP + create NAT GW + private RT")
	eipOut, err := c.EC2.AllocateAddress(&ec2.AllocateAddressInput{
		Domain: aws.String("vpc"),
	})
	require.NoError(t, err, "allocate-address vpc")
	natAllocID := aws.StringValue(eipOut.AllocationId)
	natPubIP := aws.StringValue(eipOut.PublicIp)
	require.NotEmpty(t, natAllocID, "EIP AllocationId empty")
	eipReleased := false
	t.Cleanup(func() {
		if !eipReleased {
			_, _ = c.EC2.ReleaseAddress(&ec2.ReleaseAddressInput{
				AllocationId: aws.String(natAllocID),
			})
		}
	})
	harness.Detail(t, "eip", natPubIP, "alloc", natAllocID)

	natOut, err := c.EC2.CreateNatGateway(&ec2.CreateNatGatewayInput{
		SubnetId:     aws.String(pubSubnetID),
		AllocationId: aws.String(natAllocID),
	})
	require.NoError(t, err, "create-nat-gateway")
	natGWID := aws.StringValue(natOut.NatGateway.NatGatewayId)
	require.NotEmpty(t, natGWID, "NatGatewayId empty")
	natDeleted := false
	t.Cleanup(func() {
		if !natDeleted {
			_, _ = c.EC2.DeleteNatGateway(&ec2.DeleteNatGatewayInput{
				NatGatewayId: aws.String(natGWID),
			})
			_ = waitForNATGatewayStateBest(c, natGWID, "deleted", 5*time.Minute)
		}
	})
	harness.Detail(t, "nat_gw", natGWID)
	waitForNATGatewayStateFatal(t, c, natGWID, "available")

	// Private route table + association + 0.0.0.0/0 -> NAT GW
	privRTOut, err := c.EC2.CreateRouteTable(&ec2.CreateRouteTableInput{
		VpcId: aws.String(vpcID),
	})
	require.NoError(t, err, "create-route-table (private)")
	privRTBID := aws.StringValue(privRTOut.RouteTable.RouteTableId)
	require.NotEmpty(t, privRTBID, "private RouteTableId empty")
	rtbDeleted := false
	t.Cleanup(func() {
		if !rtbDeleted {
			_, _ = c.EC2.DeleteRouteTable(&ec2.DeleteRouteTableInput{
				RouteTableId: aws.String(privRTBID),
			})
		}
	})

	rtbAssocOut, err := c.EC2.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(privRTBID),
		SubnetId:     aws.String(privSubnetID),
	})
	require.NoError(t, err, "associate private RT")
	rtbAssocID := aws.StringValue(rtbAssocOut.AssociationId)
	assocReleased := false
	t.Cleanup(func() {
		if !assocReleased {
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
	routeDeleted := false
	t.Cleanup(func() {
		if !routeDeleted {
			_, _ = c.EC2.DeleteRoute(&ec2.DeleteRouteInput{
				RouteTableId:         aws.String(privRTBID),
				DestinationCidrBlock: aws.String("0.0.0.0/0"),
			})
		}
	})

	// --- Step 7: Verify internet via NAT GW from all 3 ------------------
	harness.Step(t, "Step 7 — verify internet via NAT GW (x%d)", len(privIPs))
	for i, privIP := range privIPs {
		host := hostingNodes[i]
		harness.EventuallyErr(t, func() error {
			if _, err := pingViaBastion(keyPath, bastionPubIP, privIP); err != nil {
				return fmt.Errorf("ping via NAT GW (%s on %s): %w", privIDs[i], host, err)
			}
			return nil
		}, 90*time.Second, 5*time.Second)
		harness.Detail(t, "post_nat", privIDs[i], "node", host, "reachable", true)
	}

	// --- Step 8: Cleanup NAT GW resources (inline; teardown flips flags) -
	harness.Step(t, "Step 8 — cleanup NAT GW + private RT")
	_, err = c.EC2.DeleteRoute(&ec2.DeleteRouteInput{
		RouteTableId:         aws.String(privRTBID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
	})
	require.NoError(t, err, "delete-route 0.0.0.0/0")
	routeDeleted = true

	_, err = c.EC2.DisassociateRouteTable(&ec2.DisassociateRouteTableInput{
		AssociationId: aws.String(rtbAssocID),
	})
	require.NoError(t, err, "disassociate private RT")
	assocReleased = true

	_, err = c.EC2.DeleteRouteTable(&ec2.DeleteRouteTableInput{
		RouteTableId: aws.String(privRTBID),
	})
	require.NoError(t, err, "delete private RT")
	rtbDeleted = true

	_, err = c.EC2.DeleteNatGateway(&ec2.DeleteNatGatewayInput{
		NatGatewayId: aws.String(natGWID),
	})
	require.NoError(t, err, "delete-nat-gateway")
	waitForNATGatewayStateFatal(t, c, natGWID, "deleted")
	natDeleted = true

	_, err = c.EC2.ReleaseAddress(&ec2.ReleaseAddressInput{
		AllocationId: aws.String(natAllocID),
	})
	require.NoError(t, err, "release-address")
	eipReleased = true

	// IGW detach + delete still runs as a unit at the registered t.Cleanup
	// above (igwDetached never flipped) so the IGW removal stays sequenced
	// before VPC delete on every path — success or failure.
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
// equivalent: phase 11 Step 2's 30-attempt 5s loop = 150s budget.
func waitForBastionSSH(t *testing.T, host, keyPath string, timeout time.Duration) {
	t.Helper()
	harness.Step(t, "wait bastion SSH %s", host)
	harness.EventuallyErr(t, func() error {
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
			return fmt.Errorf("ssh %s: %w\n%s", host, err, string(out))
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
