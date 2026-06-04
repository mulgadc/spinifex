//go:build e2e

package single

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"time"

	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fresh-install reachability baselines.
//
// The rest of the suite calls harness.AuthorizeSSHIngress before every SSH
// probe, so it never exercises the out-of-box default configuration. These
// two tests pin the two independent barriers a real user hits when they
// launch an instance and expect to reach it:
//
//  1. SG barrier (runDefaultSGReachabilityBaseline): an instance in the
//     fully-routed default subnet is NOT reachable from outside until an
//     ingress rule is added — AWS default-SG semantics admit only same-SG
//     members. Routes are fine; the SG is the gate.
//
//  2. Route/egress barrier (runNewVPCEgressBaseline): a brand-new VPC where
//     the 0.0.0.0/0 -> IGW route is added to the MAIN route table BEFORE the
//     public subnet is created. The subnet implicitly joins the main RT, which
//     already carries the IGW route, but CreateSubnet does not recompute the
//     per-subnet OVN egress policy — so the subnet's egress LRP is never
//     installed and the instance has no datapath to the IGW. The default VPC
//     masks this: its main RT is prebaked with the IGW route at bootstrap and
//     the vpcd drift loop reconciles any implicit-main subnets, so a subnet
//     created in it is reachable regardless of the create-time gap.
//
// Both own all their resources (dedicated SG + instance; #2 owns a whole VPC)
// so they don't perturb the singleton VM or the shared default SG.

// tcpReachable reports whether a TCP connect to host:port succeeds within
// timeout. An OVN ACL drop yields a dial timeout (no RST); a reject yields
// connection-refused — either way the connect fails and we return false.
func tcpReachable(host string, port int, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// launchBaselineInstance launches one instance into subnetID with the given
// SGs and registers a terminate-and-wait cleanup so the VM is gone before the
// next test runs. Unlike harness.EnsureInstance (which defers teardown to
// fixture cleanup at suite end), these baselines own their VMs for the test's
// duration only — they must not perturb cluster-VM-count assertions in
// sibling tests.
func launchBaselineInstance(t *testing.T, fix *Fixture, ami, instType, keyName, subnetID string, sgIDs []string) string {
	t.Helper()
	sgs := make([]*string, 0, len(sgIDs))
	for _, id := range sgIDs {
		sgs = append(sgs, aws.String(id))
	}
	out, err := fix.AWS.EC2.RunInstances(&ec2.RunInstancesInput{
		ImageId:          aws.String(ami),
		InstanceType:     aws.String(instType),
		KeyName:          aws.String(keyName),
		SubnetId:         aws.String(subnetID),
		SecurityGroupIds: sgs,
		MinCount:         aws.Int64(1),
		MaxCount:         aws.Int64(1),
	})
	require.NoError(t, err, "RunInstances")
	require.NotEmpty(t, out.Instances, "RunInstances returned no instances")
	id := aws.StringValue(out.Instances[0].InstanceId)
	t.Cleanup(func() {
		_, _ = fix.AWS.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
			InstanceIds: []*string{aws.String(id)},
		})
		harness.WaitForInstanceState(t, fix.AWS, id, "terminated")
	})
	harness.WaitForInstanceState(t, fix.AWS, id, "running")
	return id
}

// instancePrivateIP returns the instance's primary VPC private IP.
func instancePrivateIP(t *testing.T, fix *Fixture, id string) string {
	t.Helper()
	out, err := fix.AWS.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(id)},
	})
	require.NoError(t, err, "describe-instances %s", id)
	require.NotEmpty(t, out.Reservations, "no reservations for %s", id)
	require.NotEmpty(t, out.Reservations[0].Instances, "no instances for %s", id)
	ip := aws.StringValue(out.Reservations[0].Instances[0].PrivateIpAddress)
	require.NotEmptyf(t, ip, "instance %s has no private IP", id)
	return ip
}

// sshCapture runs cmd over SSH and returns combined output + error without
// fataling, so callers can assert on the exit status (e.g. ping result).
func sshCapture(tgt harness.SSHTarget, cmd string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=5",
		"-o", "BatchMode=yes",
		"-p", strconv.Itoa(tgt.Port),
		"-i", tgt.KeyPath,
		tgt.User + "@" + tgt.Host,
		cmd,
	}
	out, err := exec.CommandContext(ctx, "ssh", args...).CombinedOutput()
	return string(out), err
}

// instancePublicIP returns the instance's routable public IP. A missing public
// IP is a hard failure: these baselines must exercise the real OVN datapath,
// and the qemu-hostfwd shortcut (which would otherwise stand in) is disabled
// suite-wide because it bypasses SG ACLs / IGW / SNAT and masks regressions.
func instancePublicIP(t *testing.T, fix *Fixture, instanceID string) string {
	t.Helper()
	out, err := fix.AWS.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	})
	require.NoError(t, err, "describe-instances %s", instanceID)
	require.NotEmpty(t, out.Reservations, "no reservations for %s", instanceID)
	require.NotEmpty(t, out.Reservations[0].Instances, "no instances for %s", instanceID)
	ip := aws.StringValue(out.Reservations[0].Instances[0].PublicIpAddress)
	if ip == "" || ip == "None" {
		t.Fatalf("instance %s has no public IP; the datapath it depends on is "+
			"broken or the subnet does not auto-assign one (hostfwd fallback is disabled)", instanceID)
	}
	return ip
}

// runDefaultSGReachabilityBaseline launches an instance into the default
// subnet behind a dedicated default-deny SG, asserts it is unreachable from
// the runner, then opens tcp/22 and asserts it becomes reachable. Confirms
// the SG — not routing — gates a fresh default-config instance.
func runDefaultSGReachabilityBaseline(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — Baseline: default-SG blocks external reach until authorized")

	vpcID, _, subnetID := harness.DiscoverDefaultVPC(t, fix.AWS)
	instType, _ := needInstanceTypeArch(t, fix)
	keyName, keyPath := needKeyPair(t, fix)
	ami := needAMI(t, fix)

	// Dedicated SG with NO ingress rules. CreateSecurityGroup seeds allow-all
	// egress, so the SSH return path is unaffected — ingress is the only gate.
	sgID := harness.EnsureSG(t, fix.Harness, vpcID, "baseline-denysg")
	harness.Detail(t, "vpc", vpcID, "subnet", subnetID, "sg", sgID)

	instanceID := launchBaselineInstance(t, fix, ami, instType, keyName, subnetID, []string{sgID})

	pubIP := instancePublicIP(t, fix, instanceID)
	harness.Detail(t, "instance", instanceID, "public_ip", pubIP)

	// Phase A — default-deny SG: tcp/22 must stay blocked. Poll for a window
	// so a slow datapath converging to "open" would still be caught.
	harness.Step(t, "asserting tcp/22 stays blocked under default-deny SG")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		require.Falsef(t, tcpReachable(pubIP, 22, 3*time.Second),
			"tcp/22 to %s connected with NO ingress rule — default SG must deny external traffic", pubIP)
		time.Sleep(3 * time.Second)
	}

	// Phase B — authorize tcp/22, then a full handshake must succeed. Proves
	// the subnet routes to the IGW and only the SG was gating reachability.
	harness.Step(t, "authorizing tcp/22 ingress, expecting reachability")
	harness.AuthorizeSSHIngress(t, fix.AWS, sgID)
	require.Truef(t, trySSHReady(pubIP, 22, keyPath, 3*time.Minute),
		"tcp/22 to %s never became reachable after authorizing ingress — "+
			"default subnet egress/IGW datapath is broken", pubIP)

	tgt := harness.SSHTarget{User: "ec2-user", Host: pubIP, Port: 22, KeyPath: keyPath}
	idOut := runSSH(t, tgt, "id")
	assert.Containsf(t, idOut, "ec2-user", "ssh id after authorize\n%s", idOut)
}

// mainRouteTableID returns the main (implicitly-associated) route table for
// vpcID — the one a subnet joins when it has no explicit RT association.
func mainRouteTableID(t *testing.T, c *harness.AWSClient, vpcID string) string {
	t.Helper()
	out, err := c.EC2.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("vpc-id"), Values: []*string{aws.String(vpcID)}},
			{Name: aws.String("association.main"), Values: []*string{aws.String("true")}},
		},
	})
	require.NoError(t, err, "describe-route-tables main vpc=%s", vpcID)
	require.NotEmptyf(t, out.RouteTables, "no main route table for vpc %s", vpcID)
	id := aws.StringValue(out.RouteTables[0].RouteTableId)
	require.NotEmpty(t, id, "main RouteTableId empty")
	return id
}

// runNewVPCEgressBaseline stands up a brand-new VPC and reproduces the IGW
// egress gap that the default-VPC path masks. The bug is order-sensitive: the
// 0.0.0.0/0 -> IGW route is added to the MAIN route table BEFORE the public
// subnet exists, so when the subnet is created it implicitly joins a main RT
// that already carries the route. CreateSubnet does not recompute per-subnet
// egress, so the subnet's OVN egress LRP is never installed and the instance
// has no datapath to the IGW. With an open SG, routing is the only remaining
// barrier, so an unreachable instance pins the gap.
//
// runVPCSubnetE2E does NOT catch this: it uses a custom RT, associates the
// subnet, then CreateRoute — so the route mutation enumerates the already-
// associated subnet and installs the LRP. The default-VPC path doesn't catch
// it either (route prebaked at bootstrap + drift-loop reconcile).
//
// PoolMode-gated like runVPCSubnetE2E: a fresh VPC needs OVN IPAM to hand the
// instance a routable public IP, which dev_networking single-node lacks.
func runNewVPCEgressBaseline(t *testing.T, fix *Fixture) {
	if !fix.PoolMode {
		t.Skip("fresh-VPC egress baseline requires pool-mode networking")
	}
	harness.Phase(t, "Single — Baseline: fresh VPC main-RT IGW egress (route before subnet)")

	c := fix.AWS
	az := needAZ(t, fix)
	instType, _ := needInstanceTypeArch(t, fix)
	keyName, keyPath := needKeyPair(t, fix)
	ami := needAMI(t, fix)

	const vpcCIDR = "10.231.0.0/16"
	const subnetCIDR = "10.231.1.0/24"

	// --- VPC ---
	vpcOut, err := c.EC2.CreateVpc(&ec2.CreateVpcInput{CidrBlock: aws.String(vpcCIDR)})
	require.NoError(t, err, "create-vpc")
	require.NotNil(t, vpcOut.Vpc, "create-vpc returned nil Vpc")
	vpcID := aws.StringValue(vpcOut.Vpc.VpcId)
	require.NotEmpty(t, vpcID, "VpcId empty")
	t.Cleanup(func() { _, _ = c.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}) })

	// --- IGW + attach ---
	igwOut, err := c.EC2.CreateInternetGateway(&ec2.CreateInternetGatewayInput{})
	require.NoError(t, err, "create-internet-gateway")
	require.NotNil(t, igwOut.InternetGateway, "create-internet-gateway returned nil InternetGateway")
	igwID := aws.StringValue(igwOut.InternetGateway.InternetGatewayId)
	require.NotEmpty(t, igwID, "InternetGatewayId empty")
	t.Cleanup(func() {
		_, _ = c.EC2.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
			InternetGatewayId: aws.String(igwID), VpcId: aws.String(vpcID),
		})
		_, _ = c.EC2.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{
			InternetGatewayId: aws.String(igwID),
		})
	})
	_, err = c.EC2.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID), VpcId: aws.String(vpcID),
	})
	require.NoError(t, err, "attach-internet-gateway")

	mainRT := mainRouteTableID(t, c, vpcID)
	harness.Detail(t, "vpc", vpcID, "igw", igwID, "main_rtb", mainRT)

	// --- Route FIRST, on the main RT, before the subnet exists ---
	harness.Step(t, "create-route 0.0.0.0/0 -> %s on main RT %s (before subnet)", igwID, mainRT)
	_, err = c.EC2.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(mainRT),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String(igwID),
	})
	require.NoError(t, err, "create-route 0.0.0.0/0 -> IGW on main RT")
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteRoute(&ec2.DeleteRouteInput{
			RouteTableId:         aws.String(mainRT),
			DestinationCidrBlock: aws.String("0.0.0.0/0"),
		})
	})

	// --- Subnet AFTER the route (implicit main-RT membership) ---
	harness.Step(t, "create-subnet %s (implicit main RT, after IGW route)", subnetCIDR)
	subOut, err := c.EC2.CreateSubnet(&ec2.CreateSubnetInput{
		VpcId:            aws.String(vpcID),
		CidrBlock:        aws.String(subnetCIDR),
		AvailabilityZone: aws.String(az),
	})
	require.NoError(t, err, "create-subnet")
	require.NotNil(t, subOut.Subnet, "create-subnet returned nil Subnet")
	subnetID := aws.StringValue(subOut.Subnet.SubnetId)
	require.NotEmpty(t, subnetID, "SubnetId empty")
	t.Cleanup(func() { _, _ = c.EC2.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String(subnetID)}) })

	_, err = c.EC2.ModifySubnetAttribute(&ec2.ModifySubnetAttributeInput{
		SubnetId:            aws.String(subnetID),
		MapPublicIpOnLaunch: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	})
	require.NoError(t, err, "modify-subnet-attribute MapPublicIpOnLaunch on %s", subnetID)

	// --- Open SG so routing is provably the only barrier ---
	sgID := harness.EnsureSG(t, fix.Harness, vpcID, "baseline-newvpc-opensg")
	harness.AuthorizeSSHIngress(t, fix.AWS, sgID)
	harness.Detail(t, "subnet", subnetID, "cidr", subnetCIDR, "sg", sgID)

	instanceID := launchBaselineInstance(t, fix, ami, instType, keyName, subnetID, []string{sgID})
	pubIP := instancePublicIP(t, fix, instanceID)
	harness.Detail(t, "instance", instanceID, "public_ip", pubIP)

	harness.Step(t, "expecting external SSH to instance in fresh-VPC public subnet")
	if !trySSHReady(pubIP, 22, keyPath, 3*time.Minute) {
		harness.DumpVPCFlowDiagnostics(t, c, instanceID,
			fmt.Sprintf("fresh-VPC egress baseline SSH timeout — vpc=%s igw=%s pub=%s", vpcID, igwID, pubIP),
			harness.VPCDiagnosticsOpts{
				ExternalIP:  pubIP,
				LogicalIP:   instancePrivateIP(t, fix, instanceID),
				ArtifactDir: fix.ArtifactDir(t),
			})
		t.Fatalf("tcp/22 to %s in fresh-VPC subnet %s never became reachable — the "+
			"per-subnet IGW egress policy was not installed for a subnet created "+
			"after the main-RT IGW route existed", pubIP, subnetID)
	}

	tgt := harness.SSHTarget{User: "ec2-user", Host: pubIP, Port: 22, KeyPath: keyPath}
	idOut := runSSH(t, tgt, "id")
	assert.Containsf(t, idOut, "ec2-user", "ssh id in fresh-VPC subnet\n%s", idOut)
}

// runSameSGComms launches two instances in the default SG (plus a dedicated
// runner-SSH SG so the runner can shell into one) and asserts that one can
// ICMP-ping the other over the VPC. ICMP between them is permitted only by the
// default SG's self-reference rule, so success proves that rule is enforced on
// the OVN datapath — the east-west counterpart to the external-reach baseline.
// No default resource is mutated (only the dedicated runner SG is authorized).
func runSameSGComms(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — Baseline: same default-SG instances communicate")

	vpcID, defSGID, subnetID := harness.DiscoverDefaultVPC(t, fix.AWS)
	instType, _ := needInstanceTypeArch(t, fix)
	keyName, keyPath := needKeyPair(t, fix)
	ami := needAMI(t, fix)

	// Dedicated SG opens tcp/22 only, so ICMP between guests still depends on
	// the default SG's self-ingress — keeps the signal unambiguous.
	runnerSG := harness.EnsureSG(t, fix.Harness, vpcID, "baseline-runnersg")
	harness.AuthorizeSSHIngress(t, fix.AWS, runnerSG)
	harness.Detail(t, "vpc", vpcID, "default_sg", defSGID, "runner_sg", runnerSG)

	srcID := launchBaselineInstance(t, fix, ami, instType, keyName, subnetID, []string{defSGID, runnerSG})
	dstID := launchBaselineInstance(t, fix, ami, instType, keyName, subnetID, []string{defSGID, runnerSG})

	dstPriv := instancePrivateIP(t, fix, dstID)

	srcInst := harness.WaitForInstanceState(t, fix.AWS, srcID, "running")
	host, port := harness.InstancePublicSSHHost(t, srcInst)
	harness.Detail(t, "src", srcID, "dst", dstID, "dst_private_ip", dstPriv, "ssh_host", host)
	waitForSSHHandshake(t, host, port, keyPath)

	tgt := harness.SSHTarget{User: "ec2-user", Host: host, Port: port, KeyPath: keyPath}
	harness.Step(t, "ping %s (%s) from %s via default-SG self-ingress", dstID, dstPriv, srcID)
	out, err := sshCapture(tgt, fmt.Sprintf("ping -c 3 -W 2 %s", dstPriv))
	require.NoErrorf(t, err,
		"intra-default-SG ping %s -> %s failed; self-ingress not enforced on the datapath\n%s",
		srcID, dstID, out)
	assert.Containsf(t, out, "0% packet loss",
		"intra-default-SG ping had loss; self-ingress datapath degraded\n%s", out)
}
