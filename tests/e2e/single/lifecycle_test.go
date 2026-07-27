//go:build e2e

package single

import (
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// sshDatapathBroken is set the first time a single-node SSH probe times
// out. Downstream SSH-dependent tests call requireSSHHealthy(t) and skip
// rather than re-running the same 3-minute timeout against the same
// broken datapath.
var sshDatapathBroken atomic.Bool

// sshReadyBudget bounds the single-node SSH-handshake probes. Baremetal boots
// q35 guests slower (OVMF + NBD disks), so the harness widens it via
// SPINIFEX_SSH_READY_TIMEOUT (Go duration); default 3m bounds shared runners.
var sshReadyBudget = resolveSSHReadyBudget(3 * time.Minute)

func resolveSSHReadyBudget(def time.Duration) time.Duration {
	if v := os.Getenv("SPINIFEX_SSH_READY_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// requireSSHHealthy skips the calling test if a prior SSH probe on this
// single-node VM has already failed. Cuts the cascade cost of a broken
// datapath from 4×3min down to one fail + four skips.
func requireSSHHealthy(t *testing.T) {
	t.Helper()
	if sshDatapathBroken.Load() {
		t.Skipf("skipping: earlier single-node SSH probe failed; " +
			"not retrying to keep suite time bounded")
	}
}

// runStopStart stops fix.Instance and waits for it to reach the
// "stopped" state, then asserts that rebooting a stopped instance is
// rejected with IncorrectInstanceState. Leaves the instance stopped so
// Phase 7a / 7b can act against the stopped state. Maps to run-e2e.sh
// ~1027–1053.
func runStopStart(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — Instance State Transitions (Stop)")

	inst, _ := needInstance(t, fix)
	instanceID := aws.StringValue(inst.InstanceId)

	harness.Step(t, "stop-instances %s", instanceID)
	_, err := fix.AWS.EC2.StopInstances(&ec2.StopInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	})
	require.NoError(t, err, "stop-instances")

	harness.WaitForInstanceState(t, fix.AWS, instanceID, "stopped")

	// Reboot of a stopped instance must be rejected — IncorrectInstanceState
	// is the AWS code bash's expect_error pins on.
	harness.Step(t, "reboot-instances on stopped instance (should fail)")
	harness.ExpectError(t, "IncorrectInstanceState", func() error {
		_, err := fix.AWS.EC2.RebootInstances(&ec2.RebootInstancesInput{
			InstanceIds: []*string{aws.String(instanceID)},
		})
		return err
	})
	harness.Detail(t, "stopped_reject", "IncorrectInstanceState")

	// Restore primary instance to running so sibling Test* don't trip on a
	// stopped singleton row.
	harness.Step(t, "start-instances %s (restore for sibling tests)", instanceID)
	_, err = fix.AWS.EC2.StartInstances(&ec2.StartInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	})
	require.NoError(t, err, "start-instances (restore)")
	harness.WaitForInstanceState(t, fix.AWS, instanceID, "running")
}

// runAttachToStoppedError creates a fresh 10 GiB volume and attempts
// to attach it to the (currently stopped) primary instance. The attach
// must fail with IncorrectInstanceState. The test volume is deleted
// in-line on success and via t.Cleanup on early failure. Maps to
// run-e2e.sh ~1054–1068.
func runAttachToStoppedError(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — Attach Volume to Stopped Instance (Error Path)")

	inst, _ := needInstance(t, fix)
	instanceID := aws.StringValue(inst.InstanceId)
	az := needAZ(t, fix)

	// Stop the primary instance so the IncorrectInstanceState assertion has
	// something to bite on, then restore it at end so sibling Test* see the
	// canonical running row.
	harness.Step(t, "stop-instances %s (precondition for 7a)", instanceID)
	_, err := fix.AWS.EC2.StopInstances(&ec2.StopInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	})
	require.NoError(t, err, "stop-instances")
	harness.WaitForInstanceState(t, fix.AWS, instanceID, "stopped")
	t.Cleanup(func() {
		_, _ = fix.AWS.EC2.StartInstances(&ec2.StartInstancesInput{
			InstanceIds: []*string{aws.String(instanceID)},
		})
		harness.WaitForInstanceState(t, fix.AWS, instanceID, "running")
	})

	harness.Step(t, "create-volume size=10 az=%s", az)
	create, err := fix.AWS.EC2.CreateVolume(&ec2.CreateVolumeInput{
		Size:             aws.Int64(10),
		AvailabilityZone: aws.String(az),
	})
	require.NoError(t, err, "create-volume")
	stoppedVolID := aws.StringValue(create.VolumeId)
	require.NotEmpty(t, stoppedVolID, "create-volume returned empty VolumeId")
	harness.Detail(t, "test_volume", stoppedVolID)

	// Best-effort cleanup if the negative assertion or anything else fails
	// before we reach the explicit delete-volume below.
	cleaned := false
	t.Cleanup(func() {
		if cleaned {
			return
		}
		_, _ = fix.AWS.EC2.DeleteVolume(&ec2.DeleteVolumeInput{
			VolumeId: aws.String(stoppedVolID),
		})
	})

	harness.WaitForVolumeState(t, fix.AWS, stoppedVolID, "available")

	harness.Step(t, "attach-volume %s -> %s (should fail)", stoppedVolID, instanceID)
	harness.ExpectError(t, "IncorrectInstanceState", func() error {
		_, err := fix.AWS.EC2.AttachVolume(&ec2.AttachVolumeInput{
			VolumeId:   aws.String(stoppedVolID),
			InstanceId: aws.String(instanceID),
			Device:     aws.String("/dev/sdg"),
		})
		return err
	})

	harness.Step(t, "delete-volume %s", stoppedVolID)
	_, err = fix.AWS.EC2.DeleteVolume(&ec2.DeleteVolumeInput{
		VolumeId: aws.String(stoppedVolID),
	})
	require.NoError(t, err, "delete-volume %s", stoppedVolID)
	cleaned = true
}

// waitForSSHReady probes a full SSH handshake (BatchMode + ConnectTimeout)
// against host:port, retrying until the daemon completes banner exchange.
// TCP-port reachability alone is insufficient — sshd accepts the connect
// while pam/cloud-init are still finishing, and the first real runSSH then
// hits "Connection timed out during banner exchange" (CI run 26034322018).
func waitForSSHReady(t *testing.T, host string, port int, keyPath string) {
	t.Helper()
	requireSSHHealthy(t)
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	harness.Step(t, "waiting for SSH handshake %s", addr)
	if !trySSHReady(host, port, keyPath, sshReadyBudget) {
		sshDatapathBroken.Store(true)
		t.Fatalf("Eventually: condition not met within %s: "+
			"[SSH handshake %s never completed] "+
			"(sticky-skip enabled for downstream)", sshReadyBudget, addr)
	}
}

// waitForSSHHandshake is an alias kept for guestchurn_test.go's call site.
func waitForSSHHandshake(t *testing.T, host string, port int, keyPath string) {
	waitForSSHReady(t, host, port, keyPath)
}

// trySSHReady mirrors waitForSSHReady but returns reachability as a bool
// instead of t.Fatal-ing on timeout. Matches run-e2e.sh ~2850–2860, which
// treats SSH-via-public-IP timeout as a WARN ("bridge not ready") rather
// than a hard test failure — new-subnet IGW SNAT/DNAT
// flow installation is timing-sensitive on shared CI runners and not part
// of the EC2 surface contract this test exists to validate.
func trySSHReady(host string, port int, keyPath string, timeout time.Duration) bool {
	if sshDatapathBroken.Load() {
		return false
	}
	deadline := time.Now().Add(timeout)
	for {
		cmd := exec.Command("ssh",
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-o", "LogLevel=ERROR",
			"-o", "ConnectTimeout=3",
			"-o", "BatchMode=yes",
			"-p", strconv.Itoa(port),
			"-i", keyPath,
			"ubuntu@"+host,
			"true")
		if cmd.Run() == nil {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Second)
	}
}
