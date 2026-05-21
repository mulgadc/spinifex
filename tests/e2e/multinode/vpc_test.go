//go:build e2e

package multinode

import (
	"regexp"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// vpcSubnetCIDR is the regex bash uses to gate subnet IP allocation —
// IPs must come from 10.200.1.0/24. Compiled once.
var vpcSubnetCIDR = regexp.MustCompile(`^10\.200\.1\.[0-9]+$`)

// runVPCNetworking is the Go port of VPC networking validation
// (run-multinode-e2e.sh:1042-1251). Stands up a fresh non-default VPC +
// subnet, launches 3 instances into the subnet, asserts each gets a unique
// PrivateIpAddress within 10.200.1.0/24, then stops + restarts all three and
// re-asserts the same IPs persist.
//
// Independent of the package-level trio (which lives on the default VPC) —
// this test needs full control over subnet allocation, so instances are
// launched, terminated, and cleaned up locally via t.Cleanup.
func runVPCNetworking(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode — VPC Networking")

	amiID := needAMI(t, fix, needArch(t, fix))
	instType, _ := needInstanceTypeArch(t, fix)
	keyName, _ := needKeyPair(t, fix)
	c := fix.AWS

	// --- VPC ---------------------------------------------------------------
	harness.Step(t, "create-vpc 10.200.0.0/16")
	vpcOut, err := c.EC2.CreateVpc(&ec2.CreateVpcInput{
		CidrBlock: aws.String("10.200.0.0/16"),
	})
	require.NoError(t, err, "create-vpc")
	require.NotNil(t, vpcOut.Vpc, "create-vpc returned nil Vpc")
	vpcID := aws.StringValue(vpcOut.Vpc.VpcId)
	require.NotEmpty(t, vpcID, "VpcId empty")
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(vpcID)})
	})
	harness.Detail(t, "vpc", vpcID, "cidr", "10.200.0.0/16")

	// --- Subnet ------------------------------------------------------------
	harness.Step(t, "create-subnet 10.200.1.0/24")
	subOut, err := c.EC2.CreateSubnet(&ec2.CreateSubnetInput{
		VpcId:     aws.String(vpcID),
		CidrBlock: aws.String("10.200.1.0/24"),
	})
	require.NoError(t, err, "create-subnet")
	require.NotNil(t, subOut.Subnet, "create-subnet returned nil Subnet")
	subnetID := aws.StringValue(subOut.Subnet.SubnetId)
	require.NotEmpty(t, subnetID, "SubnetId empty")
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String(subnetID)})
	})
	harness.Detail(t, "subnet", subnetID, "cidr", "10.200.1.0/24")

	// OVN topology programming is async — bash sleeps 2s after create-subnet
	// before launching. Mirror that pause to avoid flakes during the first
	// RunInstances.
	time.Sleep(2 * time.Second)

	// --- Launch 3 instances ------------------------------------------------
	harness.Step(t, "run-instances x3 into %s", subnetID)
	var instIDs []string
	for i := 0; i < 3; i++ {
		runOut, err := c.EC2.RunInstances(&ec2.RunInstancesInput{
			ImageId:      aws.String(amiID),
			InstanceType: aws.String(instType),
			KeyName:      aws.String(keyName),
			SubnetId:     aws.String(subnetID),
			MinCount:     aws.Int64(1),
			MaxCount:     aws.Int64(1),
		})
		require.NoErrorf(t, err, "run-instances #%d", i+1)
		require.NotEmptyf(t, runOut.Instances, "run-instances #%d: no Instances", i+1)
		id := aws.StringValue(runOut.Instances[0].InstanceId)
		require.NotEmptyf(t, id, "run-instances #%d: empty InstanceId", i+1)
		instIDs = append(instIDs, id)
		harness.Detail(t, "instance", id)
		// Per-instance cleanup — terminate so the subnet/VPC can drop in
		// later cleanup phases. Best-effort; we're already unwinding.
		idCopy := id
		t.Cleanup(func() {
			_, _ = c.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
				InstanceIds: []*string{aws.String(idCopy)},
			})
		})
	}

	harness.Step(t, "wait running x%d", len(instIDs))
	for _, id := range instIDs {
		harness.WaitForInstanceState(t, c, id, "running",
			harness.WithTimeout(60*time.Second), harness.WithPoll(2*time.Second))
	}

	// --- Initial IP assertions --------------------------------------------
	harness.Step(t, "verify PrivateIpAddress + Subnet/VPC ids")
	initialIPs := describeAndCollectIPs(t, c, instIDs, subnetID, vpcID)

	seen := map[string]bool{}
	for _, ip := range initialIPs {
		require.Regexpf(t, vpcSubnetCIDR, ip,
			"PrivateIpAddress %s not in 10.200.1.0/24", ip)
		require.Falsef(t, seen[ip], "duplicate PrivateIpAddress %s", ip)
		seen[ip] = true
	}
	harness.Detail(t, "ips_initial", initialIPs)

	// --- Stop + verify IP persists while stopped --------------------------
	harness.Step(t, "stop-instances x%d", len(instIDs))
	for _, id := range instIDs {
		_, err := c.EC2.StopInstances(&ec2.StopInstancesInput{
			InstanceIds: []*string{aws.String(id)},
		})
		require.NoErrorf(t, err, "stop-instances %s", id)
	}
	for _, id := range instIDs {
		harness.WaitForInstanceState(t, c, id, "stopped",
			harness.WithTimeout(60*time.Second), harness.WithPoll(2*time.Second))
	}

	harness.Step(t, "verify IPs persist while stopped")
	stoppedIPs := describeAndCollectIPs(t, c, instIDs, subnetID, vpcID)
	for i, id := range instIDs {
		require.Equalf(t, initialIPs[i], stoppedIPs[i],
			"%s PrivateIpAddress changed while stopped: was %s, now %s",
			id, initialIPs[i], stoppedIPs[i])
	}

	// --- Restart + verify IP persists after restart -----------------------
	harness.Step(t, "start-instances x%d", len(instIDs))
	for _, id := range instIDs {
		_, err := c.EC2.StartInstances(&ec2.StartInstancesInput{
			InstanceIds: []*string{aws.String(id)},
		})
		require.NoErrorf(t, err, "start-instances %s", id)
	}
	for _, id := range instIDs {
		harness.WaitForInstanceState(t, c, id, "running",
			harness.WithTimeout(60*time.Second), harness.WithPoll(2*time.Second))
	}

	harness.Step(t, "verify IPs persist after restart")
	restartedIPs := describeAndCollectIPs(t, c, instIDs, subnetID, vpcID)
	for i, id := range instIDs {
		require.Equalf(t, initialIPs[i], restartedIPs[i],
			"%s PrivateIpAddress changed after restart: was %s, now %s",
			id, initialIPs[i], restartedIPs[i])
	}
	harness.Detail(t, "ips_restarted", restartedIPs)
}

// describeAndCollectIPs runs DescribeInstances for each id, asserts the
// Subnet/VPC binding matches expectations, and returns PrivateIpAddress
// values in the same order as instIDs. Fatal on any missing field — bash
// also fatals on these.
func describeAndCollectIPs(t *testing.T, c *harness.AWSClient, instIDs []string, wantSubnet, wantVPC string) []string {
	t.Helper()
	ips := make([]string, 0, len(instIDs))
	for _, id := range instIDs {
		out, err := c.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
			InstanceIds: []*string{aws.String(id)},
		})
		require.NoErrorf(t, err, "describe-instances %s", id)
		require.NotEmptyf(t, out.Reservations, "describe-instances %s: no Reservations", id)
		require.NotEmptyf(t, out.Reservations[0].Instances, "describe-instances %s: no Instances", id)
		inst := out.Reservations[0].Instances[0]
		ip := aws.StringValue(inst.PrivateIpAddress)
		require.NotEmptyf(t, ip, "%s: empty PrivateIpAddress", id)
		require.Equalf(t, wantSubnet, aws.StringValue(inst.SubnetId),
			"%s: SubnetId mismatch", id)
		require.Equalf(t, wantVPC, aws.StringValue(inst.VpcId), "%s: VpcId mismatch", id)
		ips = append(ips, ip)
	}
	return ips
}

// needArch returns the discovered arch for the nano instance type.
// Convenience wrapper because needAMI takes arch, not (t, fix).
func needArch(t *testing.T, fix *Fixture) string {
	t.Helper()
	_, arch := needInstanceTypeArch(t, fix)
	return arch
}
