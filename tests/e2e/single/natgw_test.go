//go:build e2e

package single

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// phase8d_NATGateway ports run-e2e.sh ~3017–3161 (Phase 8d).
//
// Stands up its own bastion + private-instance pair in the default VPC,
// allocates a NAT Gateway in the default (public) subnet, attaches a
// 0.0.0.0/0 route on a fresh route table associated with the private
// subnet, and proves outbound egress flips on/off with the NAT GW
// lifecycle. Skipped outside pool/external mode because the gateway
// won't program OVN SNAT without an external pool.
//
// Self-contained: the bash version reused PUB/PRIV instances created by
// Phase 8b, but the Go fixture has no equivalent state (Phase 8b is a
// sibling Stage F file). Everything launched here is torn down LIFO by
// the registered t.Cleanup so a failure mid-flight still releases the
// EIP and terminates both VMs.
func phase8d_NATGateway(t *testing.T, fix *Fixture) {
	if !fix.PoolMode {
		t.Skip("Phase 8d requires pool-mode networking")
	}
	harness.Phase(t, "Phase 8d — NAT Gateway E2E")

	require.NotEmpty(t, fix.AMIID, "Phase 4 must populate fix.AMIID")
	require.NotEmpty(t, fix.InstanceType, "Phase 2 must populate fix.InstanceType")
	require.NotEmpty(t, fix.KeyName, "Phase 3 must populate fix.KeyName")
	require.NotEmpty(t, fix.KeyPath, "Phase 3 must populate fix.KeyPath")

	c := fix.AWS

	// Default VPC + default public subnet are created by admin init. The
	// public subnet has MapPublicIpOnLaunch=true and the default IGW
	// attached, so bastion launches there pick up a routable public IP
	// without extra plumbing.
	if fix.DefaultVPCID == "" {
		vpcID, sgID, subnetID := harness.DiscoverDefaultVPC(t, c)
		fix.DefaultVPCID, fix.DefaultSGID, fix.DefaultSubnetID = vpcID, sgID, subnetID
	}
	harness.Detail(t, "vpc", fix.DefaultVPCID, "pub_subnet", fix.DefaultSubnetID, "sg", fix.DefaultSGID)
	harness.AuthorizeSSHIngress(t, c, fix.DefaultSGID)

	// --- Private subnet ----------------------------------------------------
	harness.Step(t, "create-subnet 172.31.16.0/20 (private)")
	privSubOut, err := c.EC2.CreateSubnet(&ec2.CreateSubnetInput{
		VpcId:     aws.String(fix.DefaultVPCID),
		CidrBlock: aws.String("172.31.16.0/20"),
	})
	require.NoError(t, err, "create private subnet")
	privSubnetID := aws.StringValue(privSubOut.Subnet.SubnetId)
	require.NotEmpty(t, privSubnetID, "private subnet ID empty")
	require.Falsef(t, aws.BoolValue(privSubOut.Subnet.MapPublicIpOnLaunch),
		"private subnet MapPublicIpOnLaunch must default to false")
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String(privSubnetID)})
	})
	harness.Detail(t, "priv_subnet", privSubnetID)

	// --- Bastion (public subnet) -------------------------------------------
	harness.Step(t, "run-instances bastion (default subnet)")
	bastionOut, err := c.EC2.RunInstances(&ec2.RunInstancesInput{
		ImageId:      aws.String(fix.AMIID),
		InstanceType: aws.String(fix.InstanceType),
		KeyName:      aws.String(fix.KeyName),
		SubnetId:     aws.String(fix.DefaultSubnetID),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	})
	require.NoError(t, err, "run-instances bastion")
	require.NotEmpty(t, bastionOut.Instances, "bastion run-instances returned no Instances")
	bastionID := aws.StringValue(bastionOut.Instances[0].InstanceId)
	require.NotEmpty(t, bastionID, "bastion InstanceId empty")
	t.Cleanup(func() {
		_, _ = c.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
			InstanceIds: []*string{aws.String(bastionID)},
		})
		// Wait for termination so the subnet can be deleted by the later
		// cleanup. Best-effort: errors are swallowed because the test is
		// already unwinding.
		_ = waitForInstanceStateSoft(c, bastionID, "terminated", 5*time.Minute)
	})

	// --- Private instance ---------------------------------------------------
	harness.Step(t, "run-instances private (private subnet)")
	privOut, err := c.EC2.RunInstances(&ec2.RunInstancesInput{
		ImageId:      aws.String(fix.AMIID),
		InstanceType: aws.String(fix.InstanceType),
		KeyName:      aws.String(fix.KeyName),
		SubnetId:     aws.String(privSubnetID),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	})
	require.NoError(t, err, "run-instances private")
	require.NotEmpty(t, privOut.Instances, "private run-instances returned no Instances")
	privID := aws.StringValue(privOut.Instances[0].InstanceId)
	require.NotEmpty(t, privID, "private InstanceId empty")
	t.Cleanup(func() {
		_, _ = c.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
			InstanceIds: []*string{aws.String(privID)},
		})
		_ = waitForInstanceStateSoft(c, privID, "terminated", 5*time.Minute)
	})

	bastion := harness.WaitForInstanceState(t, c, bastionID, "running")
	priv := harness.WaitForInstanceState(t, c, privID, "running")

	bastionPubIP := aws.StringValue(bastion.PublicIpAddress)
	require.NotEmptyf(t, bastionPubIP, "bastion %s has no PublicIpAddress (pool mode required)", bastionID)
	privIP := aws.StringValue(priv.PrivateIpAddress)
	require.NotEmptyf(t, privIP, "private instance %s has no PrivateIpAddress", privID)
	require.Emptyf(t, aws.StringValue(priv.PublicIpAddress),
		"private instance %s unexpectedly has a public IP (got %q)",
		privID, aws.StringValue(priv.PublicIpAddress))
	harness.Detail(t, "bastion", bastionID, "bastion_ip", bastionPubIP,
		"private", privID, "private_ip", privIP)

	// --- SSH plumbing ------------------------------------------------------
	// Wait for bastion SSH handshake so the SCP below has somewhere to land.
	waitForSSHReady(t, bastionPubIP, 22, fix.KeyPath)

	bastionTgt := harness.SSHTarget{User: "ec2-user", Host: bastionPubIP, Port: 22, KeyPath: fix.KeyPath}

	// Copy the keypair to the bastion so it can hop into the private VM.
	// Matches the bash `scp ... ec2-user@$PUB_IP:/tmp/key.pem` step.
	harness.Step(t, "scp keypair -> bastion:/tmp/key.pem")
	scpKey(t, fix.KeyPath, bastionPubIP)
	_ = runSSH(t, bastionTgt, "chmod 600 /tmp/key.pem")

	// Bastion → private SSH probe (cloud-init on the private VM can lag
	// the running state by 30–40s). Polls hostname through the hop.
	privProbe := fmt.Sprintf(
		"ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null "+
			"-o LogLevel=ERROR -o ConnectTimeout=10 -o BatchMode=yes "+
			"-i /tmp/key.pem ec2-user@%s hostname",
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

	// --- Baseline: private VM has NO internet -----------------------------
	pingCmd := func() string {
		return fmt.Sprintf(
			"ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null "+
				"-o LogLevel=ERROR -o ConnectTimeout=10 -o BatchMode=yes "+
				"-i /tmp/key.pem ec2-user@%s 'ping -c 1 -W 3 8.8.8.8'",
			privIP,
		)
	}
	harness.Step(t, "baseline: private VM has no internet")
	if _, err := runSSHQuiet(bastionTgt, pingCmd()); err == nil {
		t.Fatalf("baseline ping unexpectedly succeeded — private VM has internet without NAT GW")
	}

	// --- NAT Gateway + route ----------------------------------------------
	harness.Step(t, "allocate-address (NAT EIP)")
	eipOut, err := c.EC2.AllocateAddress(&ec2.AllocateAddressInput{
		Domain: aws.String("vpc"),
	})
	require.NoError(t, err, "allocate-address vpc")
	natAllocID := aws.StringValue(eipOut.AllocationId)
	natPubIP := aws.StringValue(eipOut.PublicIp)
	require.NotEmpty(t, natAllocID, "AllocationId empty")
	eipReleased := false
	t.Cleanup(func() {
		if eipReleased {
			return
		}
		_, _ = c.EC2.ReleaseAddress(&ec2.ReleaseAddressInput{
			AllocationId: aws.String(natAllocID),
		})
	})
	harness.Detail(t, "eip", natPubIP, "alloc", natAllocID)

	harness.Step(t, "create-nat-gateway in %s", fix.DefaultSubnetID)
	natOut, err := c.EC2.CreateNatGateway(&ec2.CreateNatGatewayInput{
		SubnetId:     aws.String(fix.DefaultSubnetID),
		AllocationId: aws.String(natAllocID),
	})
	require.NoError(t, err, "create-nat-gateway")
	require.NotNil(t, natOut.NatGateway, "create-nat-gateway returned nil NatGateway")
	natGWID := aws.StringValue(natOut.NatGateway.NatGatewayId)
	require.NotEmpty(t, natGWID, "NatGatewayId empty")
	natDeleted := false
	t.Cleanup(func() {
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

	// Route table → associate → CreateRoute. Order matches the bash
	// script: the SNAT publication fires off the association, so the
	// association must exist before the route gets added.
	harness.Step(t, "create-route-table")
	rtOut, err := c.EC2.CreateRouteTable(&ec2.CreateRouteTableInput{
		VpcId: aws.String(fix.DefaultVPCID),
	})
	require.NoError(t, err, "create-route-table")
	require.NotNil(t, rtOut.RouteTable, "create-route-table returned nil RouteTable")
	natRTBID := aws.StringValue(rtOut.RouteTable.RouteTableId)
	require.NotEmpty(t, natRTBID, "RouteTableId empty")
	rtbDeleted := false
	t.Cleanup(func() {
		if rtbDeleted {
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
	natAssocID := aws.StringValue(assocOut.AssociationId)
	require.NotEmpty(t, natAssocID, "AssociationId empty")
	assocReleased := false
	t.Cleanup(func() {
		if assocReleased {
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
	routeDeleted := false
	t.Cleanup(func() {
		if routeDeleted {
			return
		}
		_, _ = c.EC2.DeleteRoute(&ec2.DeleteRouteInput{
			RouteTableId:         aws.String(natRTBID),
			DestinationCidrBlock: aws.String("0.0.0.0/0"),
		})
	})

	// --- Verify private VM CAN reach the internet now --------------------
	// OVN needs a beat to install datapath flows after SNAT publishes.
	harness.Step(t, "verify private VM reaches 8.8.8.8 via NAT GW")
	harness.EventuallyErr(t, func() error {
		out, perr := runSSHQuiet(bastionTgt, pingCmd())
		if perr != nil {
			return fmt.Errorf("ping via NAT GW: %w (out=%q)", perr, out)
		}
		return nil
	}, 2*time.Minute, 5*time.Second)

	// --- Tear NAT GW down in line; assert the egress flip --------------------
	// Mirror the bash teardown order: delete-nat-gateway → disassociate
	// route table → delete-route → delete-route-table → release-address.
	// Each step flips the corresponding cleanup latch so the deferred
	// t.Cleanup blocks become no-ops on the success path.
	harness.Step(t, "delete-nat-gateway %s", natGWID)
	_, err = c.EC2.DeleteNatGateway(&ec2.DeleteNatGatewayInput{
		NatGatewayId: aws.String(natGWID),
	})
	require.NoError(t, err, "delete-nat-gateway")

	_, err = c.EC2.DisassociateRouteTable(&ec2.DisassociateRouteTableInput{
		AssociationId: aws.String(natAssocID),
	})
	require.NoError(t, err, "disassociate-route-table")
	assocReleased = true

	_, err = c.EC2.DeleteRoute(&ec2.DeleteRouteInput{
		RouteTableId:         aws.String(natRTBID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
	})
	require.NoError(t, err, "delete-route")
	routeDeleted = true

	_, err = c.EC2.DeleteRouteTable(&ec2.DeleteRouteTableInput{
		RouteTableId: aws.String(natRTBID),
	})
	require.NoError(t, err, "delete-route-table")
	rtbDeleted = true

	waitForNATGatewayState(t, c, natGWID, "deleted")
	natDeleted = true

	_, err = c.EC2.ReleaseAddress(&ec2.ReleaseAddressInput{
		AllocationId: aws.String(natAllocID),
	})
	require.NoError(t, err, "release-address")
	eipReleased = true

	// Confirm egress is gone. OVN datapath can briefly cache the old
	// SNAT flow, so poll for the loss rather than asserting immediately.
	harness.Step(t, "verify private VM lost internet after NAT GW teardown")
	harness.EventuallyErr(t, func() error {
		if _, perr := runSSHQuiet(bastionTgt, pingCmd()); perr == nil {
			return fmt.Errorf("ping still succeeding after NAT GW deletion")
		}
		return nil
	}, 90*time.Second, 5*time.Second)
}

// waitForNATGatewayState polls DescribeNatGateways until State == target.
// Inline here rather than in harness/poll.go because Phase 8d is the only
// caller today; promote once a second consumer (Phase 8e datapath, multinode)
// shows up. Default timeout 5min — NAT GW creation is typically <30s but
// deletion under load can drag.
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

// scpKey copies the harness PEM to /tmp/key.pem on the bastion. Matches
// the bash `scp -i $KEY $KEY ec2-user@$PUB_IP:/tmp/key.pem` step — the
// private VM then accepts that key for the hop.
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
		"ec2-user@" + host + ":/tmp/key.pem",
	}
	cmd := exec.Command("scp", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("scp %s -> %s:/tmp/key.pem: %v\n%s", keyPath, host, err, string(out))
	}
}

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
	out, err := exec.Command("ssh", args...).CombinedOutput()
	return string(out), err
}
