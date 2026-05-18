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

// phase5e_CreateImage builds a custom AMI from the running instance and
// records the backing snapshot ID so Phase 9 cleanup can delete the
// snapshot before terminating the instance (otherwise DeleteOnTermination
// trips over the still-referenced snapshot). Maps to run-e2e.sh ~805–844.
func phase5e_CreateImage(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Phase 5e — CreateImage Lifecycle")
	require.NotEmpty(t, fix.InstanceID, "Phase 5 must populate fix.InstanceID")

	const customName = "e2e-custom-ami"
	const customDesc = "E2E test custom image"

	harness.Step(t, "create-image instance=%s name=%s (no-reboot)", fix.InstanceID, customName)
	create, err := fix.AWS.EC2.CreateImage(&ec2.CreateImageInput{
		InstanceId:  aws.String(fix.InstanceID),
		Name:        aws.String(customName),
		Description: aws.String(customDesc),
		NoReboot:    aws.Bool(true),
	})
	require.NoError(t, err, "create-image")
	fix.CustomAMIID = aws.StringValue(create.ImageId)
	require.NotEmpty(t, fix.CustomAMIID, "create-image returned empty ImageId")
	harness.Detail(t, "custom_ami", fix.CustomAMIID)

	harness.WaitForImageState(t, fix.AWS, fix.CustomAMIID, "available")

	harness.Step(t, "describe-images %s", fix.CustomAMIID)
	out, err := fix.AWS.EC2.DescribeImages(&ec2.DescribeImagesInput{
		ImageIds: []*string{aws.String(fix.CustomAMIID)},
	})
	require.NoError(t, err, "describe-images %s", fix.CustomAMIID)
	require.NotEmpty(t, out.Images, "no image for %s", fix.CustomAMIID)
	img := out.Images[0]

	assert.Equal(t, customName, aws.StringValue(img.Name), "custom AMI Name mismatch")
	assert.Equal(t, "available", aws.StringValue(img.State), "custom AMI State should be available")

	// Capture the backing snapshot — needed by Phase 9 cleanup (Stage G).
	for _, bdm := range img.BlockDeviceMappings {
		if bdm.Ebs == nil {
			continue
		}
		if id := aws.StringValue(bdm.Ebs.SnapshotId); id != "" {
			fix.CustomAMISnapID = id
			break
		}
	}
	if fix.CustomAMISnapID == "" {
		// Bash treats this as a non-fatal warning — replicate so a transient
		// CreateImage path that omits the snapshot ref doesn't fail the suite.
		t.Logf("WARNING: custom AMI %s has no backing snapshot ID — Stage G cleanup may be incomplete",
			fix.CustomAMIID)
	} else {
		harness.Detail(t, "custom_ami_snapshot", fix.CustomAMISnapID)
	}

	// TODO(stage-?): bash mentions verifying the predastore-side snapshot
	// config exists for the custom AMI. The actual bash doesn't do that —
	// the EC2 API check above is the sole assertion. Wire up an S3 client
	// in the harness if we ever want to enforce the predastore-side state.
}
