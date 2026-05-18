//go:build e2e

package single

import (
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

// phase7_StopStart stops fix.Instance and waits for it to reach the
// "stopped" state, then asserts that rebooting a stopped instance is
// rejected with IncorrectInstanceState. Leaves the instance stopped so
// Phase 7a / 7b can act against the stopped state. Maps to run-e2e.sh
// ~1027–1053.
func phase7_StopStart(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Phase 7 — Instance State Transitions (Stop)")
	require.NotEmpty(t, fix.InstanceID, "Phase 5 must populate fix.InstanceID")

	harness.Step(t, "stop-instances %s", fix.InstanceID)
	_, err := fix.AWS.EC2.StopInstances(&ec2.StopInstancesInput{
		InstanceIds: []*string{aws.String(fix.InstanceID)},
	})
	require.NoError(t, err, "stop-instances")

	harness.WaitForInstanceState(t, fix.AWS, fix.InstanceID, "stopped")

	// Reboot of a stopped instance must be rejected — IncorrectInstanceState
	// is the AWS code bash's expect_error pins on.
	harness.Step(t, "reboot-instances on stopped instance (should fail)")
	harness.ExpectError(t, "IncorrectInstanceState", func() error {
		_, err := fix.AWS.EC2.RebootInstances(&ec2.RebootInstancesInput{
			InstanceIds: []*string{aws.String(fix.InstanceID)},
		})
		return err
	})
	harness.Detail(t, "stopped_reject", "IncorrectInstanceState")
}

// phase7a_AttachToStoppedError creates a fresh 10 GiB volume and attempts
// to attach it to the (currently stopped) primary instance. The attach
// must fail with IncorrectInstanceState. The test volume is deleted
// in-line on success and via t.Cleanup on early failure. Maps to
// run-e2e.sh ~1054–1068.
func phase7a_AttachToStoppedError(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Phase 7a — Attach Volume to Stopped Instance (Error Path)")
	require.NotEmpty(t, fix.InstanceID, "Phase 5 must populate fix.InstanceID")
	require.NotEmpty(t, fix.AZName, "Phase 2 must populate fix.AZName")

	harness.Step(t, "create-volume size=10 az=%s", fix.AZName)
	create, err := fix.AWS.EC2.CreateVolume(&ec2.CreateVolumeInput{
		Size:             aws.Int64(10),
		AvailabilityZone: aws.String(fix.AZName),
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

	harness.Step(t, "attach-volume %s -> %s (should fail)", stoppedVolID, fix.InstanceID)
	harness.ExpectError(t, "IncorrectInstanceState", func() error {
		_, err := fix.AWS.EC2.AttachVolume(&ec2.AttachVolumeInput{
			VolumeId:   aws.String(stoppedVolID),
			InstanceId: aws.String(fix.InstanceID),
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

// phase7b_ModifyInstanceAttribute changes the (stopped) instance type to
// the same-family ".small" upsize, starts the instance, SSHes in to
// verify the new vCPU + memory budget, then stops the instance again so
// Phase 7c-pre can drive its own start/reboot cycle. Maps to run-e2e.sh
// ~1070–1177.
func phase7b_ModifyInstanceAttribute(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Phase 7b — ModifyInstanceAttribute")
	require.NotEmpty(t, fix.InstanceID, "Phase 5 must populate fix.InstanceID")
	require.NotEmpty(t, fix.InstanceType, "Phase 2 must populate fix.InstanceType")
	require.NotEmpty(t, fix.KeyPath, "Phase 3 must populate fix.KeyPath")

	// Bash strips the ".nano" suffix and appends ".small" — same family,
	// more RAM at matching vCPU. Avoids xlarge (16 GiB) which the CI host
	// can't satisfy.
	if !strings.HasSuffix(fix.InstanceType, ".nano") {
		t.Fatalf("phase7b: expected fix.InstanceType to end with .nano, got %q", fix.InstanceType)
	}
	modifyType := strings.TrimSuffix(fix.InstanceType, ".nano") + ".small"
	harness.Detail(t, "from_type", fix.InstanceType, "to_type", modifyType)

	// Look up expected vCPUs / memory for the upsized type so we can
	// assert SSH-reported values match what the AWS surface advertises.
	typesOut, err := fix.AWS.EC2.DescribeInstanceTypes(&ec2.DescribeInstanceTypesInput{})
	require.NoError(t, err, "describe-instance-types")
	var expectedVCPUs int64
	var expectedMemMiB int64
	for _, it := range typesOut.InstanceTypes {
		if aws.StringValue(it.InstanceType) != modifyType {
			continue
		}
		if it.VCpuInfo != nil {
			expectedVCPUs = aws.Int64Value(it.VCpuInfo.DefaultVCpus)
		}
		if it.MemoryInfo != nil {
			expectedMemMiB = aws.Int64Value(it.MemoryInfo.SizeInMiB)
		}
		break
	}
	require.NotZero(t, expectedVCPUs, "%s missing VCpuInfo.DefaultVCpus", modifyType)
	require.NotZero(t, expectedMemMiB, "%s missing MemoryInfo.SizeInMiB", modifyType)
	harness.Detail(t, "expected_vcpus", expectedVCPUs, "expected_mem_mib", expectedMemMiB)

	harness.Step(t, "modify-instance-attribute %s type=%s", fix.InstanceID, modifyType)
	_, err = fix.AWS.EC2.ModifyInstanceAttribute(&ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String(fix.InstanceID),
		InstanceType: &ec2.AttributeValue{Value: aws.String(modifyType)},
	})
	require.NoError(t, err, "modify-instance-attribute")

	// Verify describe-instances reflects the new type before we start.
	descOut, err := fix.AWS.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(fix.InstanceID)},
	})
	require.NoError(t, err, "describe-instances")
	require.NotEmpty(t, descOut.Reservations, "no reservations for %s", fix.InstanceID)
	require.NotEmpty(t, descOut.Reservations[0].Instances, "no instances for %s", fix.InstanceID)
	gotType := aws.StringValue(descOut.Reservations[0].Instances[0].InstanceType)
	require.Equalf(t, modifyType, gotType,
		"ModifyInstanceAttribute did not stick: want %s got %s", modifyType, gotType)
	// Reflect the new type in the fixture so any later phase that reads
	// fix.InstanceType sees the post-modify value.
	fix.InstanceType = modifyType

	harness.Step(t, "start-instances %s", fix.InstanceID)
	_, err = fix.AWS.EC2.StartInstances(&ec2.StartInstancesInput{
		InstanceIds: []*string{aws.String(fix.InstanceID)},
	})
	require.NoError(t, err, "start-instances")

	inst := harness.WaitForInstanceState(t, fix.AWS, fix.InstanceID, "running")
	fix.Instance = inst

	// Re-discover SSH endpoint — qemu hostfwd may have rebound.
	host, port := harness.InstancePublicSSHHost(t, inst)
	fix.SSHHost, fix.SSHPort = host, port
	harness.Detail(t, "ssh_host", host, "ssh_port", port)

	waitForSSHReady(t, host, port, fix.KeyPath)
	tgt := harness.SSHTarget{User: "ec2-user", Host: host, Port: port, KeyPath: fix.KeyPath}

	harness.Step(t, "ssh nproc")
	nprocOut := strings.TrimSpace(runSSH(t, tgt, "nproc"))
	vmVCPUs, err := strconv.ParseInt(nprocOut, 10, 64)
	require.NoErrorf(t, err, "parse nproc output %q", nprocOut)
	require.Equalf(t, expectedVCPUs, vmVCPUs,
		"vCPU mismatch after modify: VM=%d expected=%d", vmVCPUs, expectedVCPUs)
	harness.Detail(t, "vm_vcpus", vmVCPUs)

	harness.Step(t, "ssh MemTotal")
	memOut := strings.TrimSpace(runSSH(t, tgt, "awk '/MemTotal/ {print $2}' /proc/meminfo"))
	vmMemKB, err := strconv.ParseInt(memOut, 10, 64)
	require.NoErrorf(t, err, "parse MemTotal output %q", memOut)
	vmMemMiB := vmMemKB / 1024
	// 15% margin for kernel-reserved memory, matching the bash threshold.
	expectedMemLow := expectedMemMiB * 85 / 100
	require.GreaterOrEqualf(t, vmMemMiB, expectedMemLow,
		"memory too low after modify: VM=%d MiB expected>=%d MiB (target %d MiB)",
		vmMemMiB, expectedMemLow, expectedMemMiB)
	harness.Detail(t, "vm_mem_mib", vmMemMiB, "threshold_mib", expectedMemLow)

	// Phase 7c-pre expects the instance running again, but the bash phase
	// ordering leaves the instance stopped between 7b and 7c-pre by
	// implicitly relying on 7c-pre to start it. We restore that contract
	// explicitly so 7c-pre can drive its own start.
	harness.Step(t, "stop-instances %s (end of 7b)", fix.InstanceID)
	_, err = fix.AWS.EC2.StopInstances(&ec2.StopInstancesInput{
		InstanceIds: []*string{aws.String(fix.InstanceID)},
	})
	require.NoError(t, err, "stop-instances (end of 7b)")
	harness.WaitForInstanceState(t, fix.AWS, fix.InstanceID, "stopped")
}

// phase7cPre_Reboot starts the (stopped) primary instance, captures its
// pre-reboot private IP, issues a reboot, asserts the API never reports a
// non-running state during a short polling window, waits for SSH to come
// back, and verifies the guest actually rebooted (uptime < 120s) without
// changing its private IP. Leaves the instance running. Maps to
// run-e2e.sh ~1178–1236.
func phase7cPre_Reboot(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Phase 7c-pre — Reboot Running Instance")
	require.NotEmpty(t, fix.InstanceID, "Phase 5 must populate fix.InstanceID")
	require.NotEmpty(t, fix.KeyPath, "Phase 3 must populate fix.KeyPath")

	// Bring instance back up from the stopped state left by Phase 7b.
	harness.Step(t, "start-instances %s", fix.InstanceID)
	_, err := fix.AWS.EC2.StartInstances(&ec2.StartInstancesInput{
		InstanceIds: []*string{aws.String(fix.InstanceID)},
	})
	require.NoError(t, err, "start-instances")
	inst := harness.WaitForInstanceState(t, fix.AWS, fix.InstanceID, "running")
	fix.Instance = inst

	host, port := harness.InstancePublicSSHHost(t, inst)
	fix.SSHHost, fix.SSHPort = host, port
	waitForSSHReady(t, host, port, fix.KeyPath)

	// Capture pre-reboot private IP for the post-reboot identity check.
	preDesc, err := fix.AWS.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(fix.InstanceID)},
	})
	require.NoError(t, err, "describe-instances (pre-reboot)")
	require.NotEmpty(t, preDesc.Reservations, "no reservations for %s", fix.InstanceID)
	require.NotEmpty(t, preDesc.Reservations[0].Instances, "no instances for %s", fix.InstanceID)
	preRebootIP := aws.StringValue(preDesc.Reservations[0].Instances[0].PrivateIpAddress)
	harness.Detail(t, "pre_reboot_private_ip", preRebootIP)

	harness.Step(t, "reboot-instances %s", fix.InstanceID)
	_, err = fix.AWS.EC2.RebootInstances(&ec2.RebootInstancesInput{
		InstanceIds: []*string{aws.String(fix.InstanceID)},
	})
	require.NoError(t, err, "reboot-instances")

	// Bash polls 10× at 1s checking the state stays "running" — EC2's
	// reboot semantics don't transition the instance state at all.
	for i := 0; i < 10; i++ {
		out, err := fix.AWS.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
			InstanceIds: []*string{aws.String(fix.InstanceID)},
		})
		require.NoError(t, err, "describe-instances during reboot poll %d", i)
		require.NotEmpty(t, out.Reservations[0].Instances, "instance disappeared during reboot")
		state := aws.StringValue(out.Reservations[0].Instances[0].State.Name)
		require.Equalf(t, "running", state,
			"instance unexpectedly left running state during reboot: %s", state)
		time.Sleep(1 * time.Second)
	}

	// SSH endpoint may have rebound after the guest restart (qemu hostfwd
	// can shift), so re-discover.
	descPost, err := fix.AWS.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(fix.InstanceID)},
	})
	require.NoError(t, err, "describe-instances (post-reboot)")
	require.NotEmpty(t, descPost.Reservations[0].Instances, "no instances post-reboot")
	postInst := descPost.Reservations[0].Instances[0]
	fix.Instance = postInst
	host, port = harness.InstancePublicSSHHost(t, postInst)
	fix.SSHHost, fix.SSHPort = host, port
	waitForSSHReady(t, host, port, fix.KeyPath)

	tgt := harness.SSHTarget{User: "ec2-user", Host: host, Port: port, KeyPath: fix.KeyPath}
	harness.Step(t, "ssh uptime")
	uptimeOut := strings.TrimSpace(runSSH(t, tgt, "cat /proc/uptime | cut -d. -f1"))
	uptimeSecs, err := strconv.ParseInt(uptimeOut, 10, 64)
	require.NoErrorf(t, err, "parse uptime output %q", uptimeOut)
	require.LessOrEqualf(t, uptimeSecs, int64(120),
		"guest uptime %ds is > 120s — reboot may not have occurred", uptimeSecs)
	harness.Detail(t, "uptime_secs", uptimeSecs)

	postRebootIP := aws.StringValue(postInst.PrivateIpAddress)
	assert.Equalf(t, preRebootIP, postRebootIP,
		"PrivateIpAddress changed across reboot: %s -> %s", preRebootIP, postRebootIP)

	// Leave instance running — Phase 7c launches sibling instances next,
	// and Phase 8 expects the primary instance to still be up.
}

// phase7c_RunInstancesMultiCount launches 2 sibling instances in a single
// RunInstances call, waits for both to reach "running", then terminates
// them and waits for both to reach "terminated". The primary fix.Instance
// is untouched and remains running for Phase 8. Maps to run-e2e.sh
// ~1238–1296.
func phase7c_RunInstancesMultiCount(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Phase 7c — RunInstances with MinCount/MaxCount > 1")
	require.NotEmpty(t, fix.AMIID, "Phase 4 must populate fix.AMIID")
	require.NotEmpty(t, fix.InstanceType, "Phase 2 must populate fix.InstanceType")
	require.NotEmpty(t, fix.KeyName, "Phase 3 must populate fix.KeyName")

	harness.Step(t, "run-instances ami=%s type=%s count=2", fix.AMIID, fix.InstanceType)
	out, err := fix.AWS.EC2.RunInstances(&ec2.RunInstancesInput{
		ImageId:      aws.String(fix.AMIID),
		InstanceType: aws.String(fix.InstanceType),
		KeyName:      aws.String(fix.KeyName),
		MinCount:     aws.Int64(2),
		MaxCount:     aws.Int64(2),
	})
	require.NoError(t, err, "run-instances --count 2")
	require.Lenf(t, out.Instances, 2,
		"expected 2 instances from run-instances, got %d", len(out.Instances))

	multiIDs := []string{
		aws.StringValue(out.Instances[0].InstanceId),
		aws.StringValue(out.Instances[1].InstanceId),
	}
	require.NotEmpty(t, multiIDs[0], "first sibling InstanceId empty")
	require.NotEmpty(t, multiIDs[1], "second sibling InstanceId empty")
	harness.Detail(t, "sibling_1", multiIDs[0], "sibling_2", multiIDs[1])

	// Always tear down the siblings — pre-register before any blocking
	// wait so a t.Fatal on state polling still triggers cleanup.
	terminated := false
	t.Cleanup(func() {
		if terminated {
			return
		}
		_, _ = fix.AWS.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
			InstanceIds: aws.StringSlice(multiIDs),
		})
	})

	for _, id := range multiIDs {
		harness.WaitForInstanceState(t, fix.AWS, id, "running")
	}

	harness.Step(t, "terminate-instances %s %s", multiIDs[0], multiIDs[1])
	_, err = fix.AWS.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
		InstanceIds: aws.StringSlice(multiIDs),
	})
	require.NoError(t, err, "terminate-instances")

	for _, id := range multiIDs {
		harness.WaitForInstanceState(t, fix.AWS, id, "terminated")
	}
	terminated = true
}

// waitForSSHReady probes a full SSH handshake (BatchMode + ConnectTimeout)
// against host:port, retrying until the daemon completes banner exchange.
// TCP-port reachability alone is insufficient — sshd accepts the connect
// while pam/cloud-init are still finishing, and the first real runSSH then
// hits "Connection timed out during banner exchange" (CI run 26034322018).
// Proper hoist to harness.WaitForSSH is tracked under mulga-siv-101.
func waitForSSHReady(t *testing.T, host string, port int, keyPath string) {
	t.Helper()
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	harness.Step(t, "waiting for SSH handshake %s", addr)
	harness.Eventually(t, func() bool {
		cmd := exec.Command("ssh",
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-o", "LogLevel=ERROR",
			"-o", "ConnectTimeout=5",
			"-o", "BatchMode=yes",
			"-p", strconv.Itoa(port),
			"-i", keyPath,
			"ec2-user@"+host,
			"true")
		return cmd.Run() == nil
	}, 3*time.Minute, 3*time.Second, fmt.Sprintf("SSH handshake %s never completed", addr))
}

// waitForSSHHandshake is an alias kept for instance_test.go's call site.
func waitForSSHHandshake(t *testing.T, host string, port int, keyPath string) {
	waitForSSHReady(t, host, port, keyPath)
}
