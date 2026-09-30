//go:build e2e

package single

import (
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
