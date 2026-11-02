//go:build e2e

package instancerecovery

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

const (
	// pendingWatchdogTimeout mirrors vm.PendingWatchdogTimeout. The guest is
	// older than this before anything relaunches it, which the auto-recovery
	// test's guest never was.
	pendingWatchdogTimeout = 5 * time.Minute

	// agedPast is how far past the watchdog timeout the guest has to be. A
	// minute is a full watchdog tick, so a stale clock is seen at least once.
	agedPast = pendingWatchdogTimeout + time.Minute

	// survivalHold is how long the relaunched guest is watched afterwards. It
	// spans three watchdog ticks, and a stuck-pending kill had always happened
	// within one tick of the guest starting.
	survivalHold = 3 * time.Minute

	tokenMarker = "spx-e2e-token: "
)

// tokenUserData writes a token to the root volume once and prints it from the
// volume at every boot, so a console naming it after a relaunch proves the
// guest booted from the disk it had. Read by API: routed-NAT guests have no IP.
func tokenUserData(token string) string {
	return `#!/bin/bash
[ -s /var/lib/spx-e2e-token ] || { echo ` + token + ` > /var/lib/spx-e2e-token; sync; }
cat > /etc/systemd/system/spx-e2e-token.service <<'EOF'
[Unit]
Description=print the e2e token from the root volume
After=multi-user.target
[Service]
Type=oneshot
ExecStart=/bin/sh -c 'echo "` + tokenMarker + `$(cat /var/lib/spx-e2e-token)" > /dev/console'
[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now spx-e2e-token.service
`
}

// runRecoveryKeepsAnOlderInstanceAndItsDisk proves recovery and a later start
// of an instance older than the pending watchdog keep it, its root volume and
// its data, which ADR-0003:S7 requires and a fresh guest never tested.
func runRecoveryKeepsAnOlderInstanceAndItsDisk(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Recovery keeps an instance older than the pending watchdog, and its disk")
	recoveryWaitActive(t, fix)

	token := newToken(t)
	victim, instanceID := recoveryGuestOnAPeer(t, fix, tokenUserData(token))
	harness.Detail(t, "victim_node", victim.Name)
	harness.Detail(t, "instance", instanceID)

	requireTokenOnConsole(t, fix, instanceID, token, "first boot")
	rootVolume := rootVolumeID(t, fix, instanceID)
	harness.Detail(t, "root_volume", rootVolume)

	waitUntilOlderThan(t, fix, instanceID, agedPast)

	harness.Step(t, "stop the spinifex services on %s, the node %s runs on", victim.Name, instanceID)
	harness.StopNode(t, victim)
	t.Cleanup(func() { harness.StartNode(t, victim) })
	fix.Cluster.WaitNATSPeers(t, 1, harness.WithTimeout(60*time.Second),
		harness.WithPoll(2*time.Second), harness.WithSkipNodes(victim.Name))

	harness.Step(t, "wait for a survivor to relaunch %s", instanceID)
	var hosting []harness.Node
	require.Eventuallyf(t, func() bool {
		hosting = harness.InstanceHostingNodes(t, fix.Cluster, instanceID, []string{victim.Name})
		return len(hosting) > 0
	}, recoveryBudget, 15*time.Second,
		"%s never came back on a survivor after %s stopped answering\n%s",
		instanceID, victim.Name, lazyWhy(func() string { return recoveryWhy(t, fix, victim, instanceID) }))
	harness.Detail(t, "recovered_onto", hosting[0].Name)

	requireSurvives(t, fix, instanceID, rootVolume, "after recovery onto "+hosting[0].Name)
	requireTokenOnConsole(t, fix, instanceID, token, "boot after recovery")

	harness.Step(t, "bring %s back", victim.Name)
	harness.StartNode(t, victim)
	harness.WaitNodeServiceReady(t, victim, harness.WithTimeout(3*time.Minute))
	fix.Cluster.WaitNATSPeers(t, 2, harness.WithTimeout(2*time.Minute), harness.WithPoll(2*time.Second))

	// The other path that relaunches a claimed record: a start of an instance
	// whose last launch is older than the watchdog's timeout.
	waitUntilOlderThan(t, fix, instanceID, agedPast)
	before := launchTime(t, fix, instanceID)

	harness.Step(t, "stop and start %s", instanceID)
	_, err := fix.AWS.EC2.StopInstances(&ec2.StopInstancesInput{InstanceIds: []*string{aws.String(instanceID)}})
	require.NoError(t, err, "StopInstances %s", instanceID)
	harness.WaitForInstanceState(t, fix.AWS, instanceID, "stopped")
	_, err = fix.AWS.EC2.StartInstances(&ec2.StartInstancesInput{InstanceIds: []*string{aws.String(instanceID)}})
	require.NoError(t, err, "StartInstances %s", instanceID)
	harness.WaitForInstanceState(t, fix.AWS, instanceID, "running")

	require.Truef(t, launchTime(t, fix, instanceID).After(before),
		"%s kept LaunchTime %s across a start; AWS reports the time it was last launched", instanceID, before)
	requireSurvives(t, fix, instanceID, rootVolume, "after a start")
	requireTokenOnConsole(t, fix, instanceID, token, "boot after the start")
}

// requireSurvives watches the instance for survivalHold and fails on the first
// sign it is being torn down or its root volume is gone.
func requireSurvives(t *testing.T, fix *Fixture, instanceID, rootVolume, when string) {
	t.Helper()
	harness.Step(t, "watch %s and %s for %s %s", instanceID, rootVolume, survivalHold, when)
	deadline := time.Now().Add(survivalHold)
	for time.Now().Before(deadline) {
		state, reason := instanceState(t, fix, instanceID)
		require.Containsf(t, []string{"pending", "running"}, state,
			"ADR-0003:S7: %s went to %s %s (reason %q); an instance that has run must never be "+
				"terminated by a failure nobody asked to act on", instanceID, state, when, reason)

		_, err := fix.AWS.EC2.DescribeVolumes(&ec2.DescribeVolumesInput{VolumeIds: []*string{aws.String(rootVolume)}})
		if aerr, ok := err.(awserr.Error); ok && aerr.Code() == "InvalidVolume.NotFound" {
			t.Fatalf("ADR-0003:S7: %s, the root volume of %s, was deleted %s", rootVolume, instanceID, when)
		}
		require.NoErrorf(t, err, "DescribeVolumes %s", rootVolume)
		time.Sleep(15 * time.Second)
	}
	state, _ := instanceState(t, fix, instanceID)
	require.Equalf(t, "running", state, "%s is %s at the end of the hold %s", instanceID, state, when)
}

func requireTokenOnConsole(t *testing.T, fix *Fixture, instanceID, token, when string) {
	t.Helper()
	harness.Step(t, "wait for %s to print its token from disk on the %s", instanceID, when)
	var last string
	require.Eventuallyf(t, func() bool {
		console, err := harness.InstanceConsole(fix.AWS, instanceID)
		if err != nil {
			return false
		}
		last = console
		return strings.Contains(console, tokenMarker+token)
	}, recoveryDiskLoadBudget, 10*time.Second,
		"%s did not print %q on the %s, so it did not boot from the disk it wrote\nconsole tail:\n%s",
		instanceID, tokenMarker+token, when, recoveryTail(last, 2000))
}

func waitUntilOlderThan(t *testing.T, fix *Fixture, instanceID string, age time.Duration) {
	t.Helper()
	wait := time.Until(launchTime(t, fix, instanceID).Add(age))
	if wait <= 0 {
		return
	}
	harness.Step(t, "let %s run for %s past its launch, longer than the pending watchdog allows a launch", instanceID, age)
	time.Sleep(wait)
}

func describeInstance(t *testing.T, fix *Fixture, instanceID string) *ec2.Instance {
	t.Helper()
	out, err := fix.AWS.EC2.DescribeInstances(&ec2.DescribeInstancesInput{InstanceIds: []*string{aws.String(instanceID)}})
	require.NoError(t, err, "DescribeInstances %s", instanceID)
	require.Len(t, out.Reservations, 1, "DescribeInstances %s", instanceID)
	require.Len(t, out.Reservations[0].Instances, 1, "DescribeInstances %s", instanceID)
	return out.Reservations[0].Instances[0]
}

func instanceState(t *testing.T, fix *Fixture, instanceID string) (state, reason string) {
	t.Helper()
	inst := describeInstance(t, fix, instanceID)
	if inst.StateReason != nil {
		reason = aws.StringValue(inst.StateReason.Message)
	}
	return aws.StringValue(inst.State.Name), reason
}

func launchTime(t *testing.T, fix *Fixture, instanceID string) time.Time {
	t.Helper()
	inst := describeInstance(t, fix, instanceID)
	require.NotNil(t, inst.LaunchTime, "%s has no LaunchTime", instanceID)
	return aws.TimeValue(inst.LaunchTime)
}

func rootVolumeID(t *testing.T, fix *Fixture, instanceID string) string {
	t.Helper()
	inst := describeInstance(t, fix, instanceID)
	root := aws.StringValue(inst.RootDeviceName)
	for _, bdm := range inst.BlockDeviceMappings {
		if aws.StringValue(bdm.DeviceName) == root && bdm.Ebs != nil {
			return aws.StringValue(bdm.Ebs.VolumeId)
		}
	}
	t.Fatalf("%s has no EBS volume at its root device %q: %v", instanceID, root, inst.BlockDeviceMappings)
	return ""
}

func newToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return fmt.Sprintf("tok-%s", hex.EncodeToString(b))
}
