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

// runCreateImage builds a custom AMI from the running instance and
// records the backing snapshot ID so Phase 9 cleanup can delete the
// snapshot before terminating the instance (otherwise DeleteOnTermination
// trips over the still-referenced snapshot). Maps to run-e2e.sh ~805–844.
func runCreateImage(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — CreateImage Lifecycle")

	inst, _ := needInstance(t, fix)
	instanceID := aws.StringValue(inst.InstanceId)

	const customName = "e2e-custom-ami"
	const customDesc = "E2E test custom image"

	harness.Step(t, "create-image instance=%s name=%s (no-reboot)", instanceID, customName)
	customAMIID := ensureCustomAMI(t, fix, instanceID, customName, customDesc)
	require.NotEmpty(t, customAMIID, "ensureCustomAMI returned empty ImageId")
	harness.Detail(t, "custom_ami", customAMIID)

	harness.Step(t, "describe-images %s", customAMIID)
	out, err := fix.AWS.EC2.DescribeImages(&ec2.DescribeImagesInput{
		ImageIds: []*string{aws.String(customAMIID)},
	})
	require.NoError(t, err, "describe-images %s", customAMIID)
	require.NotEmpty(t, out.Images, "no image for %s", customAMIID)
	img := out.Images[0]

	assert.Equal(t, customName, aws.StringValue(img.Name), "custom AMI Name mismatch")
	assert.Equal(t, "available", aws.StringValue(img.State), "custom AMI State should be available")

	var customAMISnapID string
	for _, bdm := range img.BlockDeviceMappings {
		if bdm.Ebs == nil {
			continue
		}
		if id := aws.StringValue(bdm.Ebs.SnapshotId); id != "" {
			customAMISnapID = id
			break
		}
	}
	if customAMISnapID == "" {
		t.Logf("WARNING: custom AMI %s has no backing snapshot ID", customAMIID)
	} else {
		harness.Detail(t, "custom_ami_snapshot", customAMISnapID)
	}

	// TODO(stage-?): bash mentions verifying the predastore-side snapshot
	// config exists for the custom AMI. The actual bash doesn't do that —
	// the EC2 API check above is the sole assertion. Wire up an S3 client
	// in the harness if we ever want to enforce the predastore-side state.
}
