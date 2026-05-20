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

// phase5_LaunchInstance discovers the default VPC, opens tcp/22 on the
// default SG, and starts a single nano instance via EnsureInstance so
// downstream callers (phase 5a–5f, 6, 7) inherit the memoized ID. Maps to
// run-e2e.sh ~257–343.
func phase5_LaunchInstance(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Phase 5 — Instance Lifecycle")
	require.NotEmpty(t, fix.AMIID, "Phase 4 must populate fix.AMIID")
	require.NotEmpty(t, fix.InstanceType, "Phase 2 must populate fix.InstanceType")
	require.NotEmpty(t, fix.KeyName, "Phase 3 must populate fix.KeyName")

	def := harness.EnsureDefaultVPC(t, fix.Harness)
	require.NotEmpty(t, def.SGID, "default SG ID required")
	harness.Detail(t, "vpc", def.VPCID, "sg", def.SGID, "subnet", def.SubnetID)
	harness.AuthorizeSSHIngress(t, fix.AWS, def.SGID)

	harness.Step(t, "run-instances ami=%s type=%s key=%s", fix.AMIID, fix.InstanceType, fix.KeyName)
	fix.InstanceID = harness.EnsureInstance(t, fix.Harness, harness.InstanceSpec{
		AMIID:        fix.AMIID,
		InstanceType: fix.InstanceType,
		KeyName:      fix.KeyName,
		SubnetID:     def.SubnetID,
		SGID:         def.SGID,
	})
	require.NotEmpty(t, fix.InstanceID, "EnsureInstance returned empty InstanceId")
	harness.Detail(t, "instance", fix.InstanceID)

	// Re-describe so downstream phases get the populated *ec2.Instance —
	// EnsureInstance only returns the ID, but phase5a-ii needs BDMs +
	// network info to derive the root volume and SSH endpoint.
	descOut, err := fix.AWS.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(fix.InstanceID)},
	})
	require.NoError(t, err, "describe-instances %s", fix.InstanceID)
	require.NotEmpty(t, descOut.Reservations, "no reservations for %s", fix.InstanceID)
	require.NotEmpty(t, descOut.Reservations[0].Instances, "no instances for %s", fix.InstanceID)
	inst := descOut.Reservations[0].Instances[0]
	fix.Instance = inst

	// Root volume: the BDM whose DeviceName matches RootDeviceName. Fall
	// back to the first BDM if RootDeviceName is empty (some drivers don't
	// echo it back).
	rootDev := aws.StringValue(inst.RootDeviceName)
	for _, bdm := range inst.BlockDeviceMappings {
		if rootDev != "" && aws.StringValue(bdm.DeviceName) != rootDev {
			continue
		}
		if bdm.Ebs != nil {
			fix.RootVolumeID = aws.StringValue(bdm.Ebs.VolumeId)
			break
		}
	}
	if fix.RootVolumeID == "" && len(inst.BlockDeviceMappings) > 0 && inst.BlockDeviceMappings[0].Ebs != nil {
		fix.RootVolumeID = aws.StringValue(inst.BlockDeviceMappings[0].Ebs.VolumeId)
	}
	require.NotEmpty(t, fix.RootVolumeID, "could not resolve root volume from BlockDeviceMappings")
	harness.Detail(t, "root_volume", fix.RootVolumeID, "root_device", rootDev)
}

// phase5aPre_ClusterStats re-runs `spx get vms` now that a VM is running.
// Single-node only — multinode uses a different cluster surface. Maps to
// run-e2e.sh ~345–353.
func phase5aPre_ClusterStats(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Phase 5a-pre — Cluster Stats CLI (with running VM)")
	if fix.Env.Mode != harness.ModeSingle {
		t.Skipf("Phase 5a-pre is single-node only (mode=%s)", fix.Env.Mode)
	}
	require.NotEmpty(t, fix.InstanceID, "Phase 5 must populate fix.InstanceID")

	vms := harness.SpxGetVMs(t)
	assert.Containsf(t, vms, fix.InstanceID,
		"spx get vms should list running instance %s\n%s", fix.InstanceID, vms)
}

// phase5a_Metadata round-trips DescribeInstances and asserts the basic
// metadata fields match what RunInstances saw. Maps to run-e2e.sh ~355–379.
func phase5a_Metadata(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Phase 5a — Instance Metadata Validation")
	require.NotEmpty(t, fix.InstanceID, "Phase 5 must populate fix.InstanceID")

	out, err := fix.AWS.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(fix.InstanceID)},
	})
	require.NoError(t, err, "describe-instances %s", fix.InstanceID)
	require.NotEmpty(t, out.Reservations, "no reservations for %s", fix.InstanceID)
	require.NotEmpty(t, out.Reservations[0].Instances, "no instances for %s", fix.InstanceID)
	inst := out.Reservations[0].Instances[0]

	assert.Equal(t, fix.InstanceType, aws.StringValue(inst.InstanceType), "InstanceType mismatch")
	assert.Equal(t, fix.KeyName, aws.StringValue(inst.KeyName), "KeyName mismatch")
	assert.Equal(t, fix.AMIID, aws.StringValue(inst.ImageId), "ImageId mismatch")
	assert.GreaterOrEqual(t, len(inst.BlockDeviceMappings), 1,
		"expected at least 1 BlockDeviceMapping, got %d", len(inst.BlockDeviceMappings))

	harness.Detail(t,
		"type", aws.StringValue(inst.InstanceType),
		"key", aws.StringValue(inst.KeyName),
		"image", aws.StringValue(inst.ImageId),
		"bdms", len(inst.BlockDeviceMappings),
	)
}

// phase5aii_SSHProbe waits for SSH to become reachable, then runs `id`,
// `lsblk`, and `hostname` against the guest to confirm the VM booted and
// presents the expected root volume. Maps to run-e2e.sh ~381–463.
func phase5aii_SSHProbe(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Phase 5a-ii — SSH Connectivity & Volume Verification")
	require.NotNil(t, fix.Instance, "Phase 5 must populate fix.Instance")
	require.NotEmpty(t, fix.KeyPath, "Phase 3 must populate fix.KeyPath")
	require.NotEmpty(t, fix.RootVolumeID, "Phase 5 must populate fix.RootVolumeID")

	host, port := harness.InstancePublicSSHHost(t, fix.Instance)
	fix.SSHHost, fix.SSHPort = host, port
	harness.Detail(t, "ssh_host", host, "ssh_port", port)

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	harness.Step(t, "waiting for SSH handshake %s", addr)
	waitForSSHHandshake(t, host, port, fix.KeyPath)
	_ = addr

	tgt := harness.SSHTarget{User: "ec2-user", Host: host, Port: port, KeyPath: fix.KeyPath}

	harness.Step(t, "ssh id")
	idOut := runSSH(t, tgt, "id")
	assert.Containsf(t, idOut, "ec2-user", "ssh id should report ec2-user\n%s", idOut)

	harness.Step(t, "lsblk root-volume cross-check vs API")
	guestGiB := harness.LsblkRootGiB(t, tgt)

	vols, err := fix.AWS.EC2.DescribeVolumes(&ec2.DescribeVolumesInput{
		VolumeIds: []*string{aws.String(fix.RootVolumeID)},
	})
	require.NoError(t, err, "describe-volumes %s", fix.RootVolumeID)
	require.NotEmpty(t, vols.Volumes, "no volume for %s", fix.RootVolumeID)
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
	shortID := strings.TrimPrefix(fix.InstanceID, "i-")
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	if strings.Contains(hn, shortID) {
		harness.Detail(t, "hostname", hn, "matches_short_id", shortID)
	} else {
		t.Logf("hostname %q does not contain short id %q (non-fatal)", hn, shortID)
	}
}

// phase5aiii_ConsoleOutput verifies GetConsoleOutput round-trips the
// instance ID. The payload itself is base64 cloud-init output; bash doesn't
// decode it and neither do we. Maps to run-e2e.sh ~465–478.
func phase5aiii_ConsoleOutput(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Phase 5a-iii — Console Output")
	require.NotEmpty(t, fix.InstanceID, "Phase 5 must populate fix.InstanceID")

	out, err := fix.AWS.EC2.GetConsoleOutput(&ec2.GetConsoleOutputInput{
		InstanceId: aws.String(fix.InstanceID),
	})
	require.NoError(t, err, "get-console-output %s", fix.InstanceID)
	require.Equal(t, fix.InstanceID, aws.StringValue(out.InstanceId),
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
