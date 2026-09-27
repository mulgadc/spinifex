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
	recoveryConfigFile = "/etc/spinifex/spinifex.toml"
	recoveryConfigBak  = "/var/tmp/spinifex.toml.e2e-instance-recovery"

	// Recovery defaults off, so every node has to be told. Appended rather than
	// edited in place: the section does not exist in an installed config, and a
	// duplicate key would be the config loader's problem rather than ours.
	recoveryEnableTOML = `printf '\n[recovery]\nenabled = true\n'`

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

	// diskLoadUserData starts a guest writing to its root volume and never
	// stopping. An idle guest issues no I/O that reaches the object store, so
	// freezing the store leaves it running and this test proves nothing;
	// measured on a real cluster, an idle guest never paused at all.
	//
	// It is cloud-init rather than SSH into the guest on purpose: routed-NAT
	// clusters give guests no public address, so a test that needed to log in
	// could not run on half the cells.
	diskLoadUserData = `#!/bin/bash
cat > /usr/local/sbin/spx-e2e-diskload <<'EOF'
#!/bin/bash
while true; do
  dd if=/dev/urandom of=/var/tmp/spx-e2e-load bs=1M count=32 oflag=direct conv=fsync 2>/dev/null
  sync
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
	recoveryEnable(t, fix)

	victim, instanceID := recoveryGuestOnAPeer(t, fix, "")
	harness.Detail(t, "victim_node", victim.Name)
	harness.Detail(t, "instance", instanceID)

	harness.Step(t, "stop the spinifex services on %s — the guest keeps running, nothing drains it", victim.Name)
	harness.StopNode(t, victim)
	t.Cleanup(func() { harness.StartNode(t, victim) })

	fix.Cluster.WaitNATSPeers(t, 1, harness.WithTimeout(60*time.Second),
		harness.WithPoll(2*time.Second), harness.WithSkipNodes(victim.Name))

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
	cli := harness.AWSClientForGateway(t, fix.Env, fix.Cluster.Nodes[0])
	harness.WaitForInstanceState(t, cli, instanceID, "running")

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
	recoveryEnable(t, fix)

	victim, instanceID := recoveryGuestOnAPeer(t, fix, diskLoadUserData)
	harness.Detail(t, "victim_node", victim.Name)
	harness.Detail(t, "instance", instanceID)

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

// recoveryEnable turns recovery on across the cluster and restarts each daemon
// to read it, restoring both when the test ends.
//
// Every node, not just the survivors: the flag is read locally and any node may
// be the one that claims. The restart is what makes the settle window start
// here rather than at boot, which is also what makes the window observable.
func recoveryEnable(t *testing.T, fix *Fixture) {
	t.Helper()
	harness.Step(t, "enable [recovery] on all %d nodes and restart their daemons", len(fix.Cluster.Nodes))

	for _, node := range fix.Cluster.Nodes {
		if already := strings.TrimSpace(recoveryRun(t, node,
			"sudo grep -c '^\\[recovery\\]' "+recoveryConfigFile+" || true")); already != "0" {
			continue
		}
		recoveryRun(t, node, "sudo cp -a "+recoveryConfigFile+" "+recoveryConfigBak)
		recoveryRun(t, node, recoveryEnableTOML+" | sudo tee -a "+recoveryConfigFile+" >/dev/null")

		restore := node
		t.Cleanup(func() {
			if _, err := recoveryRunErr(restore, "test -e "+recoveryConfigBak); err != nil {
				return
			}
			recoveryRun(t, restore, "sudo cp -a "+recoveryConfigBak+" "+recoveryConfigFile+
				" && sudo rm -f "+recoveryConfigBak+" && sudo systemctl restart spinifex-daemon")
		})
	}

	for _, node := range fix.Cluster.Nodes {
		recoveryRun(t, node, "sudo systemctl restart spinifex-daemon")
	}
	for _, node := range fix.Cluster.Nodes {
		harness.WaitNodeServiceReady(t, node, harness.WithTimeout(3*time.Minute))
	}

	// The reconciler is inert until it has seen every peer live, so a run that
	// started measuring before that would be measuring the settle window.
	harness.Step(t, "wait for every daemon to report recovery active")
	for _, node := range fix.Cluster.Nodes {
		n := node
		require.Eventuallyf(t, func() bool {
			out, err := recoveryRunErr(n, "sudo journalctl -u spinifex-daemon --no-pager -n 500")
			return err == nil && strings.Contains(out, "recovery has seen every peer live")
		}, 3*time.Minute, 5*time.Second,
			"%s never reported recovery active, so nothing below would be a test of it", n.Name)
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

	signal := func(sig string) {
		for _, node := range fix.Cluster.Nodes {
			recoveryRun(t, node,
				"PID=$(systemctl show spinifex-predastore -p MainPID --value); "+
					"[ \"$PID\" != 0 ] && sudo kill -"+sig+" \"$PID\"")
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
