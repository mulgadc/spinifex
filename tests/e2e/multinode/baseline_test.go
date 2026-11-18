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

// sameSGDiagTimeout bounds one whole dumpSameSGDiag capture across every node.
const sameSGDiagTimeout = 90 * time.Second

// baselineLaunch launches one instance with the given SGs into subnetID,
// registers a terminate-and-wait cleanup, and returns the instance ID once "running".
func baselineLaunch(t *testing.T, fix *Fixture, amiID, instType, keyName, subnetID string, sgIDs []string, opts ...func(*ec2.RunInstancesInput)) string {
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
	for _, opt := range opts {
		opt(input)
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
	return sshCaptureWithin(30*time.Second, pem, user, host, port, cmd)
}

// sshCaptureWithin is sshCapture bounded by limit instead of 30 s.
func sshCaptureWithin(limit time.Duration, pem, user, host string, port int, cmd string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	out, err := harness.RunGuestSSH(ctx, harness.SSHTarget{
		User: user, Host: host, Port: port, KeyPath: pem,
	}, cmd)
	return string(out), err
}

// pingResult is the outcome of pingUntil. elapsed excludes time spent in the
// first-failure hook, so it measures the datapath and not the diagnostics.
type pingResult struct {
	out       string
	converged bool
	attempts  int
	elapsed   time.Duration
}

// pingUntil runs attempt until it succeeds by the deadline or the budget is
// spent. Each attempt gets the time left; a success that completes after the
// deadline does not count. Time in onFirstFail extends the deadline.
func pingUntil(budget time.Duration, attempt func(left time.Duration) (string, bool), onFirstFail func(),
	now func() time.Time, pause func(time.Duration)) pingResult {
	start := now()
	deadline := start.Add(budget)
	var paused time.Duration
	var res pingResult
	for {
		left := deadline.Sub(now())
		if left <= 0 {
			res.elapsed = now().Sub(start) - paused
			return res
		}
		res.attempts++
		var ok bool
		res.out, ok = attempt(left)
		if ok && !now().After(deadline) {
			res.converged = true
			res.elapsed = now().Sub(start) - paused
			return res
		}
		if res.attempts == 1 && onFirstFail != nil {
			hookStart := now()
			onFirstFail()
			d := now().Sub(hookStart)
			deadline = deadline.Add(d)
			paused += d
		}
		if left := deadline.Sub(now()); left > 0 {
			pause(min(2*time.Second, left))
		}
	}
}

// pingConvergedCrossHost probes dstPriv over ICMP from the source guest, retrying
// until the cross-chassis datapath converges (0% loss) or timeout elapses. The
// default SG self-ingress rule matches on $<pg>_ip4, an address set ovn-northd
// derives from port-group membership, so the peer only becomes reachable once
// that set has propagated to the remote chassis and ovn-controller has installed
// the flows there. Returns the last output, whether it converged, the attempt
// count and how long convergence took, not counting onFirstFail.
func pingConvergedCrossHost(keyPath, host string, port int, dstPriv string, timeout time.Duration, onFirstFail func()) (string, bool, int, time.Duration) {
	res := pingUntil(timeout, func(left time.Duration) (string, bool) {
		out, err := sshCaptureWithin(left, keyPath, "ubuntu", host, port,
			fmt.Sprintf("ping -c 3 -W 2 -w %d %s", max(1, int(left/time.Second)), dstPriv))
		return out, err == nil && strings.Contains(out, " 0% packet loss")
	}, onFirstFail, time.Now, time.Sleep)
	return res.out, res.converged, res.attempts, res.elapsed
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
		// Never list xfrm state without nokeys: it prints the SA keys into artifacts.
		{"xfrm-state", "sudo ip xfrm state list nokeys 2>&1 || true"},
		{"xfrm-policy", "sudo ip xfrm policy list 2>&1 || true"},
		{"xfrm-sysctl", "grep -H . /proc/sys/net/core/xfrm_acq_expires /proc/sys/net/core/xfrm_larval_drop 2>&1 || true"},
		{"ipsec-status", "sudo ipsec statusall 2>&1 || sudo ipsec status 2>&1 || true"},
		{"ipsec-guard", "sudo nft list table inet spinifex_ipsec_guard 2>&1 || true"},
	}

	// One bound for the whole capture, so a hung node cannot stall the test.
	capture, cancelCapture := context.WithTimeout(context.Background(), sameSGDiagTimeout)
	defer cancelCapture()
	ssh := harness.NewPeerSSH()
	for _, n := range fix.Cluster.Nodes {
		for _, p := range probes {
			header := fmt.Sprintf("# tag=%s host=%s cmd=%s\n", tag, n.Addr, p.cmd)
			if capture.Err() != nil {
				harness.DumpFile(t, artifactDir, fmt.Sprintf("samesg-%s-%s-%s.log", tag, n.Name, p.name),
					[]byte(header+"# (skipped: diagnostic capture bound reached)\n"))
				continue
			}
			ctx, cancel := context.WithTimeout(capture, 20*time.Second)
			out, err := ssh.Run(ctx, n.Addr, p.cmd)
			cancel()
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
		header := fmt.Sprintf("# tag=%s guest=%s cmd=%s\n", tag, srcID, p.cmd)
		captureEnd, _ := capture.Deadline()
		left := time.Until(captureEnd)
		if left <= 0 {
			harness.DumpFile(t, artifactDir, fmt.Sprintf("samesg-%s-guest-%s.log", tag, p.name),
				[]byte(header+"# (skipped: diagnostic capture bound reached)\n"))
			continue
		}
		out, err := sshCaptureWithin(min(30*time.Second, left), keyPath, "ubuntu", host, port, p.cmd)
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

	// Dedicated SG for runner SSH keeps ICMP dependent on the default SG self-ingress.
	pair := launchCrossNodePair(t, fix, "baseline-runnersg")
	src, dst := pair.src, pair.dst
	keyPath, host, port, dstPriv := pair.keyPath, pair.sshHost, pair.sshPort, pair.dstPrivateIP

	// A fresh tunnel's SA can take one kernel acquire lifetime to come up. Wait
	// for it on its own budget so the ping budget below times the datapath only.
	harness.Step(t, "wait for the IPsec SAs between %s and %s", src.node.Name, dst.node.Name)
	if missing := waitCrossHostSAs(t, src.node, dst.node); len(missing) > 0 {
		dumpSameSGDiag(t, fix, "ipsec", keyPath, host, port, src.id, dst.id, dstPriv)
		t.Fatalf("IPsec SA not established within %s for %s; the cross-host datapath was not tested",
			ipsecSAReadyTimeout, strings.Join(missing, "; "))
	}

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
