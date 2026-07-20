//go:build e2e

package single

import (
	"bytes"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runInstanceClusterStats re-runs `spx get vms` now that a VM is running.
// Single-node only — multinode uses a different cluster surface. Maps to
// run-e2e.sh ~345–353.
func runInstanceClusterStats(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — Cluster Stats CLI (with running VM)")
	if fix.Env.Mode != harness.ModeSingle {
		t.Skipf("Phase 5a-pre is single-node only (mode=%s)", fix.Env.Mode)
	}

	inst, _ := needInstance(t, fix)
	instanceID := aws.StringValue(inst.InstanceId)

	vms := harness.SpxGetVMs(t)
	assert.Containsf(t, vms, instanceID,
		"spx get vms should list running instance %s\n%s", instanceID, vms)
}

// runInstanceMetadata round-trips DescribeInstances and asserts the basic
// metadata fields match what RunInstances saw. Maps to run-e2e.sh ~355–379.
func runInstanceMetadata(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — Instance Metadata Validation")

	want, _ := needInstance(t, fix)
	instanceID := aws.StringValue(want.InstanceId)
	expectedType, _ := needInstanceTypeArch(t, fix)
	expectedKey, _ := needKeyPair(t, fix)
	expectedAMI := needAMI(t, fix)

	out, err := fix.AWS.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	})
	require.NoError(t, err, "describe-instances %s", instanceID)
	require.NotEmpty(t, out.Reservations, "no reservations for %s", instanceID)
	require.NotEmpty(t, out.Reservations[0].Instances, "no instances for %s", instanceID)
	inst := out.Reservations[0].Instances[0]

	assert.Equal(t, expectedType, aws.StringValue(inst.InstanceType), "InstanceType mismatch")
	assert.Equal(t, expectedKey, aws.StringValue(inst.KeyName), "KeyName mismatch")
	assert.Equal(t, expectedAMI, aws.StringValue(inst.ImageId), "ImageId mismatch")
	assert.GreaterOrEqual(t, len(inst.BlockDeviceMappings), 1,
		"expected at least 1 BlockDeviceMapping, got %d", len(inst.BlockDeviceMappings))

	harness.Detail(t,
		"type", aws.StringValue(inst.InstanceType),
		"key", aws.StringValue(inst.KeyName),
		"image", aws.StringValue(inst.ImageId),
		"bdms", len(inst.BlockDeviceMappings),
	)
}

// runSSHProbe waits for SSH to become reachable, then runs `id`,
// `lsblk`, and `hostname` against the guest to confirm the VM booted and
// presents the expected root volume. Maps to run-e2e.sh ~381–463.
func runSSHProbe(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — SSH Connectivity Phase 5a-ii — SSH Connectivity & Volume Verification Volume Verification")

	inst, rootVolumeID := needInstance(t, fix)
	_, keyPath := needKeyPair(t, fix)
	instanceID := aws.StringValue(inst.InstanceId)

	host, port := harness.InstancePublicSSHHost(t, inst)
	harness.Detail(t, "ssh_host", host, "ssh_port", port)

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	harness.Step(t, "waiting for SSH handshake %s", addr)
	waitForSSHHandshake(t, host, port, keyPath)
	_ = addr

	tgt := harness.SSHTarget{User: "ubuntu", Host: host, Port: port, KeyPath: keyPath}

	harness.Step(t, "ssh id")
	idOut := runSSH(t, tgt, "id")
	assert.Containsf(t, idOut, "ubuntu", "ssh id should report ubuntu\n%s", idOut)

	harness.Step(t, "lsblk root-volume cross-check vs API")
	guestGiB := harness.LsblkRootGiB(t, tgt)

	vols, err := fix.AWS.EC2.DescribeVolumes(&ec2.DescribeVolumesInput{
		VolumeIds: []*string{aws.String(rootVolumeID)},
	})
	require.NoError(t, err, "describe-volumes %s", rootVolumeID)
	require.NotEmpty(t, vols.Volumes, "no volume for %s", rootVolumeID)
	apiGiB := int(aws.Int64Value(vols.Volumes[0].Size))

	// lsblk rounds down — bash treats equality as required, but on backing
	// stores where the VM's view is a hair under the API size the rounding
	// loses 1 GiB. Allow ±1 to match the bash intent without flaking on it.
	diff := guestGiB - apiGiB
	if diff < 0 {
		diff = -diff
	}
	assert.LessOrEqualf(t, diff, 1,
		"root volume size mismatch: guest=%d GiB api=%d GiB", guestGiB, apiGiB)
	harness.Detail(t, "guest_gib", guestGiB, "api_gib", apiGiB)

	harness.Step(t, "ssh hostname")
	hn := strings.TrimSpace(runSSH(t, tgt, "hostname"))
	// Bash uses `spinifex-vm-<first 8 hex chars of instance ID>` and treats
	// a missing prefix as a non-fatal warning. Replicate the soft check via
	// t.Logf rather than asserting — the spx hostname format is not part of
	// the EC2 surface contract.
	shortID := strings.TrimPrefix(instanceID, "i-")
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	if strings.Contains(hn, shortID) {
		harness.Detail(t, "hostname", hn, "matches_short_id", shortID)
	} else {
		t.Logf("hostname %q does not contain short id %q (non-fatal)", hn, shortID)
	}
}

// runConsoleOutput verifies GetConsoleOutput round-trips the
// instance ID. The payload itself is base64 cloud-init output; bash doesn't
// decode it and neither do we. Maps to run-e2e.sh ~465–478.
func runConsoleOutput(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — Console Output")

	inst, _ := needInstance(t, fix)
	instanceID := aws.StringValue(inst.InstanceId)

	out, err := fix.AWS.EC2.GetConsoleOutput(&ec2.GetConsoleOutputInput{
		InstanceId: aws.String(instanceID),
	})
	require.NoError(t, err, "get-console-output %s", instanceID)
	require.Equal(t, instanceID, aws.StringValue(out.InstanceId),
		"GetConsoleOutput returned a different InstanceId")
	harness.Detail(t,
		"instance", aws.StringValue(out.InstanceId),
		"has_output", out.Output != nil && aws.StringValue(out.Output) != "",
	)
}

// runSSH is a thin wrapper around `ssh` matching the option set used by
// harness.LsblkRootGiB. Returns stdout. t.Fatal on non-zero exit so callers
// can chain assertions on the output without nil-checking err.
func runSSH(t *testing.T, tgt harness.SSHTarget, command string) string {
	t.Helper()
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=5",
		"-o", "BatchMode=yes",
		"-p", strconv.Itoa(tgt.Port),
		"-i", tgt.KeyPath,
		tgt.User + "@" + tgt.Host,
		command,
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("ssh", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("ssh %s@%s:%d %q failed: %v\nstderr: %s",
			tgt.User, tgt.Host, tgt.Port, command, err, stderr.String())
	}
	return stdout.String()
}
