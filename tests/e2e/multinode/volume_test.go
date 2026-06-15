//go:build e2e

package multinode

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// runVolumeLifecycle creates a 10GiB volume, attaches it to trio[0] at /dev/sdf,
// detaches, and deletes. Managed inline (not via EnsureVolume) to exercise the full
// CRUD cycle — Ensure* cleanup would race the in-test delete.
func runVolumeLifecycle(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode — Volume Lifecycle")

	az := needAZ(t, fix)
	trio := needInstanceTrio(t, fix)
	require.NotEmpty(t, trio, "trio required")
	target := trio[0]

	harness.Step(t, "create 10GiB volume in %s", az)
	create, err := fix.AWS.EC2.CreateVolume(&ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(az),
		Size:             aws.Int64(10),
	})
	require.NoError(t, err, "create-volume")
	volID := aws.StringValue(create.VolumeId)
	require.NotEmpty(t, volID, "create-volume returned empty VolumeId")
	harness.Detail(t, "volume", volID)

	harness.Step(t, "attach %s to %s as /dev/sdf", volID, target)
	_, err = fix.AWS.EC2.AttachVolume(&ec2.AttachVolumeInput{
		VolumeId:   aws.String(volID),
		InstanceId: aws.String(target),
		Device:     aws.String("/dev/sdf"),
	})
	require.NoError(t, err, "attach-volume")
	harness.WaitForVolumeState(t, fix.AWS, volID, "in-use",
		harness.WithTimeout(60*time.Second), harness.WithPoll(2*time.Second))

	harness.Step(t, "detach %s", volID)
	_, err = fix.AWS.EC2.DetachVolume(&ec2.DetachVolumeInput{VolumeId: aws.String(volID)})
	require.NoError(t, err, "detach-volume")
	harness.WaitForVolumeState(t, fix.AWS, volID, "available",
		harness.WithTimeout(60*time.Second), harness.WithPoll(2*time.Second))

	harness.Step(t, "delete %s", volID)
	_, err = fix.AWS.EC2.DeleteVolume(&ec2.DeleteVolumeInput{VolumeId: aws.String(volID)})
	require.NoError(t, err, "delete-volume")
}
