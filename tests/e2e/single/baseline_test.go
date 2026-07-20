//go:build e2e

package single

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// Shared reachability-probe helpers used by the merged single-node
// scenarios: tcpReachable/pingConverged/sshCapture for raw datapath probes,
// launchBaselineInstance/instancePublicIP/instancePrivateIP for launching and
// describing a scenario-owned guest, and mainRouteTableID for resolving a
// VPC's implicitly-associated route table.

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

// launchBaselineInstance launches one instance and registers a terminate-and-wait
// cleanup so the VM is gone before the next test. Not memoized — each baseline
// owns its VM for its own duration only.
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

// pingConverged probes dst over ICMP from tgt, retrying until the east-west
// datapath converges (0% loss) or timeout elapses. L2 between two freshly
// launched instances has a warm-up: ARP cannot resolve until the destination
// guest NIC is up and OVN has programmed its port flows, which lags the EC2
// "running" transition the launch waits on. A single un-retried ping races that
// window and reports a false "Destination Host Unreachable". Returns the last
// ping output and whether it ever converged.
func pingConverged(tgt harness.SSHTarget, dst string, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	var out string
	for {
		var err error
		out, err = sshCapture(tgt, fmt.Sprintf("ping -c 3 -W 2 %s", dst))
		if err == nil && strings.Contains(out, "0% packet loss") {
			return out, true
		}
		if time.Now().After(deadline) {
			return out, false
		}
		time.Sleep(2 * time.Second)
	}
}

// sshCapture runs cmd over SSH and returns combined output + error without
// fataling, so callers can assert on the exit status (e.g. ping result).
func sshCapture(tgt harness.SSHTarget, cmd string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := harness.RunGuestSSH(ctx, tgt, cmd)
	return string(out), err
}

// instancePublicIP returns the instance's routable public IP. Missing is fatal:
// baselines must exercise the real OVN datapath (hostfwd is disabled suite-wide).
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

// The route-before-subnet ordering regression guard that used to live here
// (runNewVPCEgressBaseline) now runs as the RouteBeforeSubnet stage of
// runVPCEgressPaths (vpcegress_test.go), which is the sole remaining caller
// of mainRouteTableID above.
