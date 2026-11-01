//go:build e2e

package single

import (
	"context"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

const (
	rebootLoopDefaultRounds = 10
	// rebootLoopRoundBudget is how long one reboot has to come back on a new
	// boot ID: the powerdown budget plus a relaunch retry and a nested boot.
	rebootLoopRoundBudget = 4 * time.Minute
)

// rebootLoopRequested reports whether the loop was asked for, by naming it in
// -test.run or by setting its round count. A whole-suite run skips it: ten
// reboots cost several minutes the single suite does not have.
func rebootLoopRequested() bool {
	if os.Getenv("SPINIFEX_REBOOT_LOOP_ROUNDS") != "" {
		return true
	}
	run := flag.Lookup("test.run")
	return run != nil && strings.Contains(run.Value.String(), "TestRebootLoop")
}

// runRebootLoop reboots one fresh guest through the API round after round and
// then from inside, and requires a new boot ID every time. A reboot that
// leaves the guest silent fails the round it happened in, with its console.
func runRebootLoop(t *testing.T, fix *Fixture) {
	if !rebootLoopRequested() {
		t.Skip("opt-in: name TestRebootLoop in -test.run or set SPINIFEX_REBOOT_LOOP_ROUNDS")
	}
	rounds := envPositiveIntOr("SPINIFEX_REBOOT_LOOP_ROUNDS", rebootLoopDefaultRounds)
	harness.Phase(t, "Single — reboot loop")

	instType, _ := needInstanceTypeArch(t, fix)
	keyName, keyPath := needKeyPair(t, fix)
	amiID := needAMI(t, fix)
	vpc := harness.EnsureDefaultVPC(t, fix.Harness)
	require.NotEmpty(t, vpc.SGID, "default SG ID required")
	harness.AuthorizeSSHIngress(t, fix.AWS, vpc.SGID)

	harness.Step(t, "launch fresh guest for the reboot loop")
	instanceID := launchBaselineInstance(t, fix, amiID, instType, keyName, vpc.SubnetID, []string{vpc.SGID})
	inst := describeSingletonInstance(t, fix, instanceID)
	host, port := harness.InstancePublicSSHHost(t, inst)
	sshHealth.WaitReady(t, host, port, keyPath)
	tgt := harness.SSHTarget{User: "ubuntu", Host: host, Port: port, KeyPath: keyPath}
	bootID := strings.TrimSpace(harness.RunSSH(t, tgt, "cat /proc/sys/kernel/random/boot_id"))
	require.NotEmpty(t, bootID, "fresh guest returned an empty boot ID")

	for round := 1; round <= rounds; round++ {
		harness.Step(t, "round %d/%d: reboot-instances %s", round, rounds, instanceID)
		start := time.Now()
		_, err := fix.AWS.EC2.RebootInstances(&ec2.RebootInstancesInput{
			InstanceIds: []*string{aws.String(instanceID)},
		})
		require.NoErrorf(t, err, "round %d: reboot-instances", round)
		bootID = awaitLoopBootID(t, fix, tgt, instanceID, bootID, round)
		harness.Detail(t, "round", round, "reboot_secs", int(time.Since(start).Seconds()))
	}

	harness.Step(t, "guest-initiated reboot of %s", instanceID)
	start := time.Now()
	// Detached so the SSH session returns before the guest goes down under it.
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	_, _ = harness.RunGuestSSH(ctx, tgt, "sudo systemd-run --on-active=2 systemctl reboot")
	cancel()
	awaitLoopBootID(t, fix, tgt, instanceID, bootID, rounds+1)
	harness.Detail(t, "guest_reboot_secs", int(time.Since(start).Seconds()))

	out, err := fix.AWS.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	})
	require.NoError(t, err)
	require.Equal(t, "running",
		aws.StringValue(out.Reservations[0].Instances[0].State.Name),
		"the instance stays running through every reboot")
}

// awaitLoopBootID blocks until the guest answers SSH with a boot ID other
// than previous, and returns it.
func awaitLoopBootID(t *testing.T, fix *Fixture, tgt harness.SSHTarget, instanceID, previous string, round int) string {
	t.Helper()
	deadline := time.Now().Add(rebootLoopRoundBudget)
	var lastErr error
	for {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		out, err := harness.RunGuestSSH(ctx, tgt, "cat /proc/sys/kernel/random/boot_id")
		cancel()
		lastErr = err
		if id := strings.TrimSpace(string(out)); err == nil && id != "" && id != previous {
			return id
		}
		if time.Now().After(deadline) {
			harness.DumpInstanceConsole(t, fix.AWS, instanceID, fix.ArtifactDir(t), "reboot-loop-console.log")
			t.Fatalf("round %d: guest did not come back on a new boot ID within %s (last SSH error: %v)",
				round, rebootLoopRoundBudget, lastErr)
		}
		time.Sleep(2 * time.Second)
	}
}
