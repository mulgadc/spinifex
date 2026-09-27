//go:build e2e

package instancerecovery

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// recoveryJournalThisRun reads the daemon's log for its current invocation
	// only. Scoping it that way is what lets a line logged once at startup be
	// found on a daemon that has been up for days.
	recoveryJournalThisRun = `sudo journalctl --no-pager ` +
		`_SYSTEMD_INVOCATION_ID="$(systemctl show -p InvocationID --value spinifex-daemon)"`

	// The owner has to be seen stale across two passes before anything is
	// claimed, then the volume leases have to expire before the launch can open
	// them. That is roughly a minute of unavoidable waiting before the first
	// attempt can succeed, and a boot on top of it.
	recoveryBudget = 8 * time.Minute

	// How long the guest is given to notice its storage has gone and pause.
	// Generous because nothing about it is prompt: viperblock serves what it
	// has cached, nbdkit waits out its reconnect delay, and only then does the
	// error reach QEMU. Measured at 4m47s on a three-node cluster.
	recoveryPauseBudget = 10 * time.Minute

	// How long the guest is given to boot, run cloud-init, and announce that its
	// first write landed. The announcement is the premise of the whole test, so
	// this is generous and failing it is a hard error rather than a skip.
	recoveryDiskLoadBudget = 6 * time.Minute

	// diskLoadMarker is printed to the serial console by the load driver once a
	// full 32 MiB pass has been written and synced, so it is proof the guest is
	// driving its root volume rather than a claim that it was asked to.
	diskLoadMarker = "spx-e2e-diskload: writing"

	// diskLoadUserData starts a guest writing to its root volume and never
	// stopping. An idle guest issues no I/O that reaches the object store, so
	// freezing the store leaves it running and this test proves nothing;
	// measured on a real cluster, an idle guest never paused at all.
	//
	// It is cloud-init rather than SSH into the guest on purpose: routed-NAT
	// clusters give guests no public address, so a test that needed to log in
	// could not run on half the cells. The console is readable the same way, by
	// the API, so the marker travels the same path.
	diskLoadUserData = `#!/bin/bash
cat > /usr/local/sbin/spx-e2e-diskload <<'EOF'
#!/bin/bash
while true; do
  dd if=/dev/urandom of=/var/tmp/spx-e2e-load bs=1M count=32 oflag=direct conv=fsync 2>/dev/null
  sync
  echo "spx-e2e-diskload: writing" > /dev/console
  sleep 1
done
EOF
chmod +x /usr/local/sbin/spx-e2e-diskload
systemd-run --unit=spx-e2e-diskload /usr/local/sbin/spx-e2e-diskload
`
)

// runInstanceAutoRecovery proves a guest on a node that stops answering comes
// back on a survivor with no operator action.
//
// This is the one behaviour the feature exists for, and the only honest way to
// assert it is to look for the qemu process on the other nodes: the API
// reporting an instance running says where the records think it is, not where
// it is.
func runInstanceAutoRecovery(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Instance Auto-Recovery")
	recoveryWaitActive(t, fix)

	victim, instanceID := recoveryGuestOnAPeer(t, fix, "")
	harness.Detail(t, "victim_node", victim.Name)
	harness.Detail(t, "instance", instanceID)

	cli := harness.AWSClientForGateway(t, fix.Env, fix.Cluster.Nodes[0])
	healthy := recoveryRequireStatus(t, cli, instanceID, func(s harness.InstanceStatusSummary) bool {
		return s.State == "running" && s.SystemStatus == "ok"
	}, 3*time.Minute, "state=running with system-status=ok before anything is broken")

	harness.Step(t, "stop the spinifex services on %s — the guest keeps running, nothing drains it", victim.Name)
	harness.StopNode(t, victim)
	t.Cleanup(func() { harness.StartNode(t, victim) })

	fix.Cluster.WaitNATSPeers(t, 1, harness.WithTimeout(60*time.Second),
		harness.WithPoll(2*time.Second), harness.WithSkipNodes(victim.Name))

	// The AWS-visible half of a host failure, and the reason this is asserted
	// before the recovery rather than after it: the window is real and bounded —
	// it opens when the owner's heartbeat goes stale, which is strictly before any
	// survivor may claim, and closes when the relaunch registers. A customer
	// watching DescribeInstanceStatus is told the host is impaired, which is what
	// AWS reports for exactly this fault and is all AWS ever discloses about a
	// host. Reporting ok throughout would be the more comfortable lie.
	recoveryRequireStatus(t, cli, instanceID, func(s harness.InstanceStatusSummary) bool {
		return s.SystemStatus == "impaired" || s.SystemStatus == "insufficient-data"
	}, 4*time.Minute, "system-status=impaired while its host is gone")

	harness.Step(t, "wait for a survivor to claim and relaunch %s", instanceID)
	var hosting []harness.Node
	require.Eventuallyf(t, func() bool {
		hosting = harness.InstanceHostingNodes(t, fix.Cluster, instanceID, []string{victim.Name})
		return len(hosting) > 0
	}, recoveryBudget, 15*time.Second,
		"%s never came back on a survivor after %s stopped answering\n%s",
		instanceID, victim.Name, recoveryWhy(t, fix, victim, instanceID))

	require.Lenf(t, hosting, 1, "%s is running on more than one survivor: %v",
		instanceID, recoveryNodeNames(hosting))
	harness.Detail(t, "recovered_onto", hosting[0].Name)

	harness.Step(t, "the gateway agrees the instance is running and names the new owner")
	harness.WaitForInstanceState(t, cli, instanceID, "running")

	// The other half of the parity, and the part that says the move finished
	// rather than merely started. AWS never discloses which host an instance is
	// on, so the customer-visible evidence that it moved is that the impairment
	// ended while the instance kept running — and that the AZ did not change,
	// because a recovery that crossed one would break every placement promise
	// made to the customer.
	recovered := recoveryRequireStatus(t, cli, instanceID, func(s harness.InstanceStatusSummary) bool {
		return s.State == "running" && s.SystemStatus == "ok" && s.InstanceStatus != "impaired"
	}, 5*time.Minute, "system-status back to ok on the survivor, with the instance still running")
	assert.Equalf(t, healthy.AZ, recovered.AZ,
		"%s came back in %s having been launched in %s; a recovery must not move an instance between availability zones",
		instanceID, recovered.AZ, healthy.AZ)

	harness.Step(t, "bring %s back and assert it does not relaunch what moved", victim.Name)
	harness.StartNode(t, victim)
	harness.WaitNodeServiceReady(t, victim, harness.WithTimeout(3*time.Minute))
	fix.Cluster.WaitNATSPeers(t, 2, harness.WithTimeout(2*time.Minute), harness.WithPoll(2*time.Second))

	// Give the returning node long enough to have got it wrong. Its restore runs
	// early in startup, so a second copy would already be visible here.
	time.Sleep(60 * time.Second)
	after := harness.InstanceHostingNodes(t, fix.Cluster, instanceID, nil)
	require.Lenf(t, after, 1,
		"%s is running on %v after %s returned; a returning node must drop a local copy the records say moved",
		instanceID, recoveryNodeNames(after), victim.Name)
	assert.NotEqualf(t, victim.Name, after[0].Name,
		"%s went back to %s, so the returning node relaunched it rather than forgetting it",
		instanceID, victim.Name)
}

// runInstanceRecoveryRefusesStorageFault proves the opposite behaviour, and it
// is the one a reconciler that cannot tell its faults apart gets wrong.
//
// A guest whose object store has stopped answering is paused by werror=stop
// with its failed request held. Moving it relaunches it somewhere that will
// refuse it identically, having thrown away its RAM on the way. So nothing must
// move, and when the store comes back the guest must resume where it was.
func runInstanceRecoveryRefusesStorageFault(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Recovery Refuses a Storage Fault")
	recoveryWaitActive(t, fix)

	victim, instanceID := recoveryGuestOnAPeer(t, fix, diskLoadUserData)
	harness.Detail(t, "victim_node", victim.Name)
	harness.Detail(t, "instance", instanceID)

	recoveryWaitForDiskLoad(t, fix, instanceID)

	harness.Step(t, "freeze predastore on every node — the store is now broken cluster-wide")
	restore := recoveryFreezeStore(t, fix)

	harness.Step(t, "wait for %s to pause on its storage and publish that it did", instanceID)
	require.Eventuallyf(t, func() bool {
		return recoveryGuestIsPaused(t, victim, instanceID)
	}, recoveryPauseBudget, 10*time.Second,
		"%s never paused on io-error, so this run proves nothing about a storage fault", instanceID)

	harness.Step(t, "stop the spinifex services on %s, leaving a stale owner and a broken store", victim.Name)
	harness.StopNode(t, victim)
	t.Cleanup(func() { harness.StartNode(t, victim) })

	fix.Cluster.WaitNATSPeers(t, 1, harness.WithTimeout(60*time.Second),
		harness.WithPoll(2*time.Second), harness.WithSkipNodes(victim.Name))

	// Long enough that a reconciler which was going to move it would have. Two
	// stale passes plus a lease TTL is about a minute; this is four.
	harness.Step(t, "hold for %s and assert no survivor claims it", 4*time.Minute)
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		hosting := harness.InstanceHostingNodes(t, fix.Cluster, instanceID, []string{victim.Name})
		require.Emptyf(t, hosting,
			"%s was relaunched on %v while the object store was refusing every node; "+
				"moving a guest cannot fix a store that is broken everywhere, and it costs the guest its held request",
			instanceID, recoveryNodeNames(hosting))
		time.Sleep(20 * time.Second)
	}

	harness.Step(t, "bring the store back and assert the guest resumes rather than having moved")
	restore()
	harness.StartNode(t, victim)
	harness.WaitNodeServiceReady(t, victim, harness.WithTimeout(3*time.Minute))
	fix.Cluster.WaitNATSPeers(t, 2, harness.WithTimeout(2*time.Minute), harness.WithPoll(2*time.Second))

	cli := harness.AWSClientForGateway(t, fix.Env, fix.Cluster.Nodes[0])
	harness.WaitForInstanceState(t, cli, instanceID, "running")

	require.Eventuallyf(t, func() bool {
		return len(harness.InstanceHostingNodes(t, fix.Cluster, instanceID, nil)) == 1
	}, 3*time.Minute, 15*time.Second,
		"%s is not running on exactly one node once the store returned", instanceID)
}

// recoveryWaitActive waits until every node has recovery running, and nothing
// here turns it on: there is no setting to turn on.
//
// That absence is the assertion. A cluster installed by the ordinary path has
// recovery active on every node with no config written and no daemon restarted,
// so a build that reintroduced a switch would fail here rather than quietly
// leaving a dead node's guests down.
//
// Every node, not just the survivors, because any of them may be the one that
// claims. The reconciler is also inert until it has seen every peer live, so a
// run that started measuring before that would be measuring the settle window.
func recoveryWaitActive(t *testing.T, fix *Fixture) {
	t.Helper()
	harness.Step(t, "wait for all %d nodes to report recovery active, with no config written", len(fix.Cluster.Nodes))

	for _, node := range fix.Cluster.Nodes {
		n := node
		require.Eventuallyf(t, func() bool {
			out, err := recoveryRunErr(n, recoveryJournalThisRun)
			return err == nil && strings.Contains(out, "recovery has seen every peer live")
		}, 6*time.Minute, 5*time.Second,
			"%s never reported recovery active, so either the settle window has not passed "+
				"or recovery is off on a cluster nothing configured — and nothing below would be a test of it", n.Name)
	}
}

// recoveryFreezeStore SIGSTOPs predastore on every node and returns the thaw,
// which is also registered as a cleanup so a failure anywhere restores it.
//
// SIGSTOP rather than stopping the unit, for the reason the storagefault suite
// gives: a stopped process holds its connections open and answers nothing,
// which is the outage that defeats code with no timeout. A clean stop refuses
// connections instead and is a different fault.
func recoveryFreezeStore(t *testing.T, fix *Fixture) func() {
	t.Helper()

	// Best effort per node, and never fatal. The thaw runs after a node has been
	// taken away, where there is no process to signal — and a node it cannot
	// reach must not stop it restoring the others, which is how a failure here
	// would leave the cluster frozen for every test after it.
	signal := func(sig string) {
		for _, node := range fix.Cluster.Nodes {
			if _, err := recoveryRunErr(node,
				"PID=$(systemctl show spinifex-predastore -p MainPID --value); "+
					"if [ \"$PID\" != 0 ]; then sudo kill -"+sig+" \"$PID\"; fi"); err != nil {
				t.Logf("predastore %s on %s: %v", sig, node.Name, err)
			}
		}
	}

	var once sync.Once
	thaw := func() { once.Do(func() { signal("CONT") }) }
	t.Cleanup(thaw)
	signal("STOP")
	return thaw
}

// recoveryGuestOnAPeer launches guests until one lands on a node other than the
// first, and returns that node and instance. A non-empty userData is delivered
// to cloud-init.
//
// Not the first node: it is the operator gateway every assertion here is made
// through, and taking it down would remove the means of observing the result
// rather than test anything.
func recoveryGuestOnAPeer(t *testing.T, fix *Fixture, userData string) (harness.Node, string) {
	t.Helper()

	instType, arch := harness.DiscoverNanoInstanceType(t, fix.Harness)
	amiID := harness.DiscoverUbuntuAMI(t, fix.Harness, arch)
	keyName, _ := harness.EnsureKeyPair(t, fix.Harness)
	def := harness.EnsureDefaultVPC(t, fix.Harness)
	require.NotEmpty(t, def.SGID, "default SG required")

	in := &ec2.RunInstancesInput{
		ImageId:          aws.String(amiID),
		InstanceType:     aws.String(instType),
		KeyName:          aws.String(keyName),
		SubnetId:         aws.String(def.SubnetID),
		SecurityGroupIds: []*string{aws.String(def.SGID)},
		MinCount:         aws.Int64(1),
		MaxCount:         aws.Int64(1),
	}
	if userData != "" {
		in.UserData = aws.String(base64.StdEncoding.EncodeToString([]byte(userData)))
	}

	for attempt := 1; attempt <= 6; attempt++ {
		// A shared fixture cannot serve this: the guest has to be on a named node
		// so that node can be taken away, and losing it is what is under test.
		// e2e:allow-create
		out, err := fix.AWS.EC2.RunInstances(in)
		if err != nil {
			if strings.Contains(err.Error(), "InsufficientInstanceCapacity") {
				time.Sleep(10 * time.Second)
				continue
			}
			t.Fatalf("launch a guest to recover: %v", err)
		}
		require.NotEmpty(t, out.Instances, "RunInstances returned no instance")

		id := aws.StringValue(out.Instances[0].InstanceId)
		t.Cleanup(func() {
			_, _ = fix.AWS.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
				InstanceIds: []*string{aws.String(id)},
			})
		})
		harness.WaitForInstanceState(t, fix.AWS, id, "running")

		host := harness.InstanceHostingNode(t, fix.Cluster, id)
		require.NotNilf(t, host, "%s is running but no node is hosting its qemu", id)
		if host.Name != fix.Cluster.Nodes[0].Name {
			return *host, id
		}
		t.Logf("recoveryGuestOnAPeer: %s landed on the gateway node, launching another", id)
	}

	t.Fatalf("no guest landed on a node other than %s in six attempts", fix.Cluster.Nodes[0].Name)
	return harness.Node{}, ""
}

// recoveryWaitForDiskLoad blocks until the guest says on its console that it has
// written and synced a pass to its root volume.
//
// This is the test's own premise, and a run that froze the store before it was
// true proved nothing while reporting a product failure: the guest was still in
// UEFI, so the only thing the frozen store could reach was the EFI varstore, and
// pflash has no werror=stop to pause on. So a guest that never says it is
// writing fails here, where the message names the real problem.
func recoveryWaitForDiskLoad(t *testing.T, fix *Fixture, instanceID string) {
	t.Helper()
	harness.Step(t, "wait for %s to boot and start driving its root volume", instanceID)

	var last string
	require.Eventuallyf(t, func() bool {
		console, err := harness.InstanceConsole(fix.AWS, instanceID)
		if err != nil {
			return false
		}
		last = console
		return strings.Contains(console, diskLoadMarker)
	}, recoveryDiskLoadBudget, 10*time.Second,
		"%s never reported %q on its console, so its load driver never wrote to the root volume "+
			"and freezing the store would prove nothing\nconsole tail:\n%s",
		instanceID, diskLoadMarker, recoveryTail(last, 2000))
}

// recoveryTail returns the last n bytes of s, for a failure message that needs
// the end of a console rather than all of it.
func recoveryTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// recoveryGuestIsPaused reports whether the node's daemon has seen this guest
// pause on an I/O error, which is what a broken object store looks like from
// the control plane. BLOCK_IO_ERROR is the QMP event and the other is the
// daemon's own line; either is the pause, and it stamps Health.IOErrorSince.
func recoveryGuestIsPaused(t *testing.T, node harness.Node, instanceID string) bool {
	t.Helper()
	out, err := recoveryRunErr(node,
		"sudo journalctl -u spinifex-daemon --no-pager -n 2000 | grep -F "+harness.ShellQuote(instanceID))
	if err != nil {
		return false
	}
	return strings.Contains(out, "BLOCK_IO_ERROR") ||
		strings.Contains(out, "Guest paused on a backend I/O error")
}

// recoveryWhy gathers what decides a recovery, for a failure message that does
// not send the reader to three journals to find out which step did not happen.
func recoveryWhy(t *testing.T, fix *Fixture, victim harness.Node, instanceID string) string {
	t.Helper()
	var b strings.Builder
	for _, node := range fix.Cluster.Nodes {
		if node.Name == victim.Name {
			continue
		}
		out, err := recoveryRunErr(node,
			"sudo journalctl -u spinifex-daemon --no-pager -n 500 | grep -iE 'recovery|claim|stopped heartbeating' | tail -20")
		if err != nil {
			out = "(" + err.Error() + ")"
		}
		fmt.Fprintf(&b, "         %s recovery log:\n%s\n", node.Name, strings.TrimSpace(out))
	}
	return b.String()
}

func recoveryNodeNames(nodes []harness.Node) []string {
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		names = append(names, n.Name)
	}
	return names
}

func recoveryRun(t *testing.T, node harness.Node, cmd string) string {
	t.Helper()
	out, err := recoveryRunErr(node, cmd)
	require.NoErrorf(t, err, "%s on %s: %s", cmd, node.Name, out)
	return out
}

func recoveryRunErr(node harness.Node, cmd string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := harness.NewPeerSSH().Run(ctx, node.Addr, cmd)
	return string(out), err
}

// recoveryRequireStatus polls DescribeInstanceStatus until want is satisfied and
// returns the frame that satisfied it, failing the test with the last frame seen.
//
// The last frame is the whole value of this over a bare Eventually: "the status
// never became impaired" is not a finding on its own, and "it stayed ok while
// its host was gone" is.
func recoveryRequireStatus(t *testing.T, cli *harness.AWSClient, instanceID string,
	want func(harness.InstanceStatusSummary) bool, budget time.Duration, describe string,
) harness.InstanceStatusSummary {
	t.Helper()
	harness.Step(t, "require %s reports %s", instanceID, describe)

	var last harness.InstanceStatusSummary
	var lastErr error
	missing := false

	require.Eventuallyf(t, func() bool {
		summary, ok, err := harness.InstanceStatus(cli, instanceID)
		lastErr, missing = err, !ok && err == nil
		if err != nil || !ok {
			return false
		}
		last = summary
		return want(summary)
	}, budget, 3*time.Second,
		"%s never reported %s\nlast frame: %s\nabsent from the answer: %v\nlast error: %v",
		instanceID, describe, last, missing, lastErr)

	harness.Detail(t, "instance_status", last.String())
	return last
}
