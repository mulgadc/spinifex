//go:build e2e

package multinode

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fresh-install reachability baseline. Run before needInstanceTrio so the
// default SG/subnet/route-table are in their pristine state.
//
//   - runMultinodeSameSGCrossHostComms: proves the default SG self-reference
//     rule spans chassis — ICMP between two instances on different nodes
//     succeeds with no added ingress rule.

// sameSGPingTimeout budgets cross-chassis datapath convergence, matching the
// single-node suite's pingConverged window.
const sameSGPingTimeout = 45 * time.Second

// baselineLaunch launches one instance with the given SGs into subnetID,
// registers a terminate-and-wait cleanup, and returns the instance ID once "running".
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
		if _, err := fix.AWS.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
			InstanceIds: []*string{aws.String(id)},
		}); err != nil {
			t.Errorf("terminate instance %s: %v", id, err)
			return
		}
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

// sshCapture runs cmd over SSH and returns combined output + error without fataling.
func sshCapture(pem, user, host string, port int, cmd string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := harness.RunGuestSSH(ctx, harness.SSHTarget{
		User: user, Host: host, Port: port, KeyPath: pem,
	}, cmd)
	return string(out), err
}

// pingConvergedCrossHost probes dstPriv over ICMP from the source guest, retrying
// until the cross-chassis datapath converges (0% loss) or timeout elapses. The
// default SG self-ingress rule matches on $<pg>_ip4, an address set ovn-northd
// derives from port-group membership, so the peer only becomes reachable once
// that set has propagated to the remote chassis and ovn-controller has installed
// the flows there. Returns the last output, whether it converged, the attempt
// count and how long convergence took.
func pingConvergedCrossHost(keyPath, host string, port int, dstPriv string, timeout time.Duration, onFirstFail func()) (string, bool, int, time.Duration) {
	start := time.Now()
	deadline := start.Add(timeout)
	var out string
	for attempt := 1; ; attempt++ {
		var err error
		out, err = sshCapture(keyPath, "ubuntu", host, port,
			fmt.Sprintf("ping -c 3 -W 2 %s", dstPriv))
		if err == nil && strings.Contains(out, "0% packet loss") {
			return out, true, attempt, time.Since(start)
		}
		if attempt == 1 && onFirstFail != nil {
			onFirstFail()
		}
		if time.Now().After(deadline) {
			return out, false, attempt, time.Since(start)
		}
		time.Sleep(2 * time.Second)
	}
}

// dumpSameSGDiag snapshots the OVN/OVS state the default-SG self-ingress rule
// depends on, from every cluster node plus the source guest, into t's artifact
// dir under the given tag. Best-effort: SSH errors are recorded, never fatal.
func dumpSameSGDiag(t *testing.T, fix *Fixture, tag, keyPath, host string, port int, srcID, dstID, dstPriv string) {
	t.Helper()
	if fix.Env == nil || fix.Cluster == nil || len(fix.Cluster.Nodes) == 0 {
		t.Logf("dumpSameSGDiag(%s): no env or cluster nodes; skipping", tag)
		return
	}
	artifactDir := fix.ArtifactDir(t)
	t.Logf("dumpSameSGDiag(%s): src=%s dst=%s dst_ip=%s nodes=%d -> %s",
		tag, srcID, dstID, dstPriv, len(fix.Cluster.Nodes), artifactDir)

	probes := []struct {
		name string
		cmd  string
	}{
		{"ovn-nb-address-set", "sudo ovn-nbctl --no-leader-only list address_set 2>&1 || true"},
		{"ovn-sb-address-set", "sudo ovn-sbctl --no-leader-only list address_set 2>&1 || true"},
		{"ovn-nb-port-group", "sudo ovn-nbctl --no-leader-only list port_group 2>&1 || true"},
		{"ovn-nb-acl", "sudo ovn-nbctl --no-leader-only list acl 2>&1 || true"},
		{"ovn-sb-port-binding", "sudo ovn-sbctl --no-leader-only list port_binding 2>&1 || true"},
		{"ovs-ofctl-br-int", "sudo ovs-ofctl dump-flows br-int 2>&1 || true"},
		{"ovs-ofctl-br-int-dst", fmt.Sprintf("sudo ovs-ofctl dump-flows br-int 2>/dev/null | grep -F %s || true", dstPriv)},
		{"ovn-sbctl-show", "sudo ovn-sbctl --no-leader-only show 2>&1 || true"},
		{"arp", "arp -an 2>&1 || true"},
	}

	ssh := harness.NewPeerSSH()
	for _, n := range fix.Cluster.Nodes {
		for _, p := range probes {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			out, err := ssh.Run(ctx, n.Addr, p.cmd)
			cancel()
			header := fmt.Sprintf("# tag=%s host=%s cmd=%s\n", tag, n.Addr, p.cmd)
			if err != nil {
				header += fmt.Sprintf("# (ssh error: %v)\n", err)
			}
			name := fmt.Sprintf("samesg-%s-%s-%s.log", tag, n.Name, p.name)
			harness.DumpFile(t, artifactDir, name, append([]byte(header), out...))
		}
	}

	guestProbes := []struct {
		name string
		cmd  string
	}{
		{"ip-neigh", "ip neigh"},
		{"ip-addr", "ip -br addr"},
		{"ip-route", "ip route"},
		{"ping-dst", fmt.Sprintf("ping -c 3 -W 2 %s 2>&1 || true", dstPriv)},
	}
	for _, p := range guestProbes {
		out, err := sshCapture(keyPath, "ubuntu", host, port, p.cmd)
		header := fmt.Sprintf("# tag=%s guest=%s cmd=%s\n", tag, srcID, p.cmd)
		if err != nil {
			header += fmt.Sprintf("# (ssh error: %v)\n", err)
		}
		name := fmt.Sprintf("samesg-%s-guest-%s.log", tag, p.name)
		harness.DumpFile(t, artifactDir, name, append([]byte(header), out...))
	}
}

// runMultinodeSameSGCrossHostComms launches two instances on different nodes sharing
// the default SG and asserts ICMP-ping succeeds across hosts. ICMP is permitted only
// by the default SG self-reference rule, proving it is enforced across chassis.
func runMultinodeSameSGCrossHostComms(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode — Baseline: same default-SG instances communicate across hosts")

	vpcID, defSGID, subnetID := harness.DiscoverDefaultVPC(t, fix.AWS)
	instType, arch := needInstanceTypeArch(t, fix)
	amiID := needAMI(t, fix, arch)
	keyName, keyPath := needKeyPair(t, fix)

	// Dedicated SG for runner SSH — opens tcp/22 only; ICMP between guests
	// still depends on the default SG self-ingress, keeping the signal clean.
	runnerSG := harness.EnsureSG(t, fix.Harness, vpcID, "baseline-runnersg")
	harness.AuthorizeSSHIngress(t, fix.AWS, runnerSG)

	// Launch instances until two land on distinct nodes. Bounded so a degenerate
	// single-node placement can't loop forever.
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
	harness.GuestSSHReady(t, host, port, "ubuntu", keyPath,
		harness.WithTimeout(3*time.Minute), harness.WithPoll(3*time.Second))

	harness.Step(t, "ping %s (%s) from %s across hosts via default-SG self-ingress", dst.id, dstPriv, src.id)
	// Snapshot OVN/OVS state on the first failed burst, while the drop is still
	// observable. A later capture only ever shows the converged steady state,
	// which cannot distinguish "rule not enforced" from "rule not yet propagated".
	out, converged, attempts, elapsed := pingConvergedCrossHost(keyPath, host, port, dstPriv,
		sameSGPingTimeout, func() {
			dumpSameSGDiag(t, fix, "firstfail", keyPath, host, port, src.id, dst.id, dstPriv)
		})
	harness.Detail(t, "ping_attempts", attempts, "ping_converged", converged,
		"ping_convergence_ms", elapsed.Milliseconds())
	if !converged {
		dumpSameSGDiag(t, fix, "final", keyPath, host, port, src.id, dst.id, dstPriv)
	}
	require.Truef(t, converged,
		"cross-host ping %s -> %s never converged within %s (%d attempts); default SG self-ingress not enforced across chassis\n%s",
		src.id, dst.id, sameSGPingTimeout, attempts, out)
	assert.Containsf(t, out, "0% packet loss",
		"cross-host ping had loss; default SG self-ingress datapath degraded\n%s", out)
	// A pass that needed most of the budget is a product signal, not a green tick:
	// the datapath took that long to converge, and only the retry hid it.
	if attempts > 1 {
		t.Logf("cross-host default-SG datapath converged only after %d attempts (%s); "+
			"sustained multi-attempt convergence indicates an OVN address-set propagation delay, not a test flake",
			attempts, elapsed.Round(time.Second))
	}
}
