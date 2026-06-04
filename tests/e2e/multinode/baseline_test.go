//go:build e2e

package multinode

import (
	"context"
	"fmt"
	"net"
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

// Fresh-install reachability baselines for the multi-node cluster. These run
// right after preflight and before any test triggers needInstanceTrio (which
// authorizes SSH on the default SG), so the default SG / subnet / route table
// are observed in their pristine, out-of-box state.
//
// Two facts they pin:
//
//   - runMultinodeDefaultSGReachabilityBaseline: an instance in the default
//     subnet behind a dedicated default-deny SG is NOT reachable from the
//     runner until tcp/22 is authorized — the SG, not routing, is the gate.
//
//   - runMultinodeSameSGCrossHostComms: two instances on DIFFERENT nodes that
//     share the default SG can reach each other over the VPC datapath with no
//     ingress rule added — the default SG's self-reference rule is enforced as
//     an OVN address set spanning chassis. The probe is ICMP, which only the
//     default SG's self-ingress permits, so the signal can't be confounded by
//     the tcp/22 rule other tests add to the default SG later.
//
// Neither mutates a default resource: dedicated SGs are authorized, the
// default SG is only read / used for membership.

// baselineLaunch launches one instance with the given SGs into subnetID and
// registers a terminate-and-wait cleanup. Returns the instance ID once
// "running". Self-cleaning so the cluster VM inventory is unperturbed for
// sibling tests.
func baselineLaunch(t *testing.T, fix *Fixture, amiID, instType, keyName, subnetID string, sgIDs []string) string {
	t.Helper()
	sgs := make([]*string, 0, len(sgIDs))
	for _, id := range sgIDs {
		sgs = append(sgs, aws.String(id))
	}
	input := &ec2.RunInstancesInput{
		ImageId:          aws.String(amiID),
		InstanceType:     aws.String(instType),
		KeyName:          aws.String(keyName),
		SubnetId:         aws.String(subnetID),
		SecurityGroupIds: sgs,
		MinCount:         aws.Int64(1),
		MaxCount:         aws.Int64(1),
	}
	var id string
	for attempt := 1; attempt <= 6; attempt++ {
		out, err := fix.AWS.EC2.RunInstances(input)
		if err == nil {
			require.NotEmpty(t, out.Instances, "RunInstances returned no instances")
			id = aws.StringValue(out.Instances[0].InstanceId)
			break
		}
		if !strings.Contains(err.Error(), "InsufficientInstanceCapacity") {
			t.Fatalf("RunInstances: %v", err)
		}
		t.Logf("baselineLaunch attempt %d: InsufficientInstanceCapacity, retrying", attempt)
		time.Sleep(10 * time.Second)
	}
	require.NotEmpty(t, id, "RunInstances never succeeded")
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
// fataling, so callers can assert on the exit status (ping success/failure).
func sshCapture(pem, user, host string, port int, cmd string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	args := []string{
		"-i", pem,
		"-p", strconv.Itoa(port),
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=5",
		"-o", "BatchMode=yes",
		"-o", "LogLevel=ERROR",
		fmt.Sprintf("%s@%s", user, host),
		cmd,
	}
	out, err := exec.CommandContext(ctx, "ssh", args...).CombinedOutput()
	return string(out), err
}

// runMultinodeDefaultSGReachabilityBaseline asserts the default-deny SG gate
// on the public-IP datapath: blocked before authorize, reachable after.
func runMultinodeDefaultSGReachabilityBaseline(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode — Baseline: default-deny SG blocks external reach until authorized")

	vpcID, _, subnetID := harness.DiscoverDefaultVPC(t, fix.AWS)
	instType, arch := needInstanceTypeArch(t, fix)
	amiID := needAMI(t, fix, arch)
	keyName, keyPath := needKeyPair(t, fix)

	sgID := harness.EnsureSG(t, fix.Harness, vpcID, "baseline-denysg")
	id := baselineLaunch(t, fix, amiID, instType, keyName, subnetID, []string{sgID})

	inst := harness.WaitForInstanceState(t, fix.AWS, id, "running")
	pubIP := aws.StringValue(inst.PublicIpAddress)
	if pubIP == "" || pubIP == "None" {
		t.Fatalf("instance %s has no public IP; the datapath it depends on is "+
			"broken or the subnet does not auto-assign one (hostfwd fallback is disabled)", id)
	}
	harness.Detail(t, "instance", id, "public_ip", pubIP, "sg", sgID)

	harness.Step(t, "asserting tcp/22 stays blocked under default-deny SG")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		conn, derr := net.DialTimeout("tcp", net.JoinHostPort(pubIP, "22"), 3*time.Second)
		if derr == nil {
			_ = conn.Close()
			t.Fatalf("tcp/22 to %s connected with NO ingress rule — default-deny SG must block external traffic", pubIP)
		}
		time.Sleep(3 * time.Second)
	}

	harness.Step(t, "authorizing tcp/22, expecting reachability")
	harness.AuthorizeSSHIngress(t, fix.AWS, sgID)
	harness.GuestSSHReady(t, pubIP, 22, "ec2-user", keyPath,
		harness.WithTimeout(3*time.Minute), harness.WithPoll(3*time.Second))

	out, err := sshCapture(keyPath, "ec2-user", pubIP, 22, "id")
	require.NoErrorf(t, err, "ssh id after authorize: %s", out)
	assert.Containsf(t, out, "ec2-user", "ssh id after authorize\n%s", out)
}

// runMultinodeSameSGCrossHostComms launches two instances on different nodes,
// both in the default SG plus a dedicated runner-SSH SG, and asserts that one
// can ICMP-ping the other over the VPC. ICMP is permitted only by the default
// SG's self-reference rule, so success proves that rule is enforced across
// chassis with no default-SG mutation.
func runMultinodeSameSGCrossHostComms(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode — Baseline: same default-SG instances communicate across hosts")

	vpcID, defSGID, subnetID := harness.DiscoverDefaultVPC(t, fix.AWS)
	instType, arch := needInstanceTypeArch(t, fix)
	amiID := needAMI(t, fix, arch)
	keyName, keyPath := needKeyPair(t, fix)

	// Dedicated SG so the runner can SSH into a probe instance without touching
	// the default SG. Opens tcp/22 only — ICMP between guests still depends on
	// the default SG's self-ingress, keeping the cross-host signal clean.
	runnerSG := harness.EnsureSG(t, fix.Harness, vpcID, "baseline-runnersg")
	harness.AuthorizeSSHIngress(t, fix.AWS, runnerSG)

	// Launch instances until two land on distinct nodes (best-effort scheduler
	// spread). Bounded so a degenerate single-node placement can't loop.
	type placed struct {
		id   string
		node string
	}
	var instances []placed
	var srcIdx, dstIdx = -1, -1
	for attempt := 0; attempt < 4 && (srcIdx < 0 || dstIdx < 0); attempt++ {
		id := baselineLaunch(t, fix, amiID, instType, keyName, subnetID, []string{defSGID, runnerSG})
		node := harness.InstanceHostingNode(t, fix.Cluster, id)
		nodeName := ""
		if node != nil {
			nodeName = node.Name
		}
		instances = append(instances, placed{id: id, node: nodeName})
		// Re-scan for a distinct-node pair.
		srcIdx, dstIdx = -1, -1
		for i := range instances {
			for j := range instances {
				if i != j && instances[i].node != "" && instances[i].node != instances[j].node {
					srcIdx, dstIdx = i, j
				}
			}
		}
	}
	if srcIdx < 0 || dstIdx < 0 {
		t.Skipf("could not place two instances on distinct nodes (got %v); scheduler colocated", instances)
	}
	src, dst := instances[srcIdx], instances[dstIdx]
	harness.Detail(t, "src", src.id, "src_node", src.node, "dst", dst.id, "dst_node", dst.node)

	dstPriv := instancePrivateIP(t, fix, dst.id)

	// Shell into the source instance via its dedicated runner-SSH SG.
	host, port := harness.GuestSSHEndpoint(t, fix.AWS, fix.Cluster, src.id)
	harness.GuestSSHReady(t, host, port, "ec2-user", keyPath,
		harness.WithTimeout(3*time.Minute), harness.WithPoll(3*time.Second))

	harness.Step(t, "ping %s (%s) from %s across hosts via default-SG self-ingress", dst.id, dstPriv, src.id)
	out, err := sshCapture(keyPath, "ec2-user", host, port,
		fmt.Sprintf("ping -c 3 -W 2 %s", dstPriv))
	require.NoErrorf(t, err,
		"cross-host ping %s -> %s failed; default SG self-ingress not enforced across chassis\n%s",
		src.id, dst.id, out)
	assert.Containsf(t, out, "0% packet loss",
		"cross-host ping had loss; default SG self-ingress datapath degraded\n%s", out)
}
