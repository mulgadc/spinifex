//go:build integration

package integration

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_ec2_snapshot "github.com/mulgadc/spinifex/spinifex/handlers/ec2/snapshot"
	"github.com/stretchr/testify/require"
)

// TestImage ports tests/e2e/single/image_test.go's runImage describe-images
// round-trip. The live test resolved a pre-staged gold-image AMI via
// needAMI (bootstrap-install.sh's `spx admin images import`); this tier has
// no such bootstrap, so it registers its own AMI against a hand-seeded
// snapshot via the real RegisterImage path instead — same production
// ImageServiceImpl.DescribeImages contract, without depending on a live
// cluster's pre-imported gold image.
func TestImage(t *testing.T) {
	t.Parallel()

	gw := StartGateway(t)
	_, store := StartImageDaemonLite(t, gw)
	ec2Cli := gw.EC2Client(t)

	const snapshotID = "snap-integration-test-image"
	require.NoError(t, handlers_ec2_snapshot.WriteSnapshotConfig(store, testImageBucket, snapshotID, &handlers_ec2_snapshot.SnapshotConfig{
		SnapshotID: snapshotID,
		VolumeID:   "vol-integration-test-image",
		VolumeSize: 8,
		State:      "completed",
		OwnerID:    gw.AccountID,
	}), "seed snapshot metadata")

	registerOut, err := ec2Cli.RegisterImage(&ec2.RegisterImageInput{
		Name:           aws.String("integration-test-ami"),
		RootDeviceName: aws.String("/dev/sda1"),
		BlockDeviceMappings: []*ec2.BlockDeviceMapping{{
			DeviceName: aws.String("/dev/sda1"),
			Ebs:        &ec2.EbsBlockDevice{SnapshotId: aws.String(snapshotID)},
		}},
	})
	require.NoError(t, err, "register-image")
	amiID := aws.StringValue(registerOut.ImageId)
	require.NotEmpty(t, amiID, "register-image returned empty ImageId")

	byID, err := ec2Cli.DescribeImages(&ec2.DescribeImagesInput{
		ImageIds: []*string{aws.String(amiID)},
	})
	require.NoErrorf(t, err, "describe-images %s", amiID)
	require.Lenf(t, byID.Images, 1, "expected exactly 1 image for %s, got %d", amiID, len(byID.Images))
	require.Equal(t, amiID, aws.StringValue(byID.Images[0].ImageId), "round-trip AMI ID mismatch")
	require.NotEmpty(t, aws.StringValue(byID.Images[0].Name), "lookup-by-ID returned empty Name")
}
