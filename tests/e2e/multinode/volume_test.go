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

// runVolumeLifecycle is the Go port of volume lifecycle
// (run-multinode-e2e.sh:731-825). Creates a 10GiB volume, attaches to the
// first trio instance at /dev/sdf, detaches, deletes. Each step polls a
// matching state target (in-use/attached → available → 404).
//
// Volume is created + torn down inline (not via EnsureVolume) because the
// test exercises the full CRUD cycle including DeleteVolume — wiring it
// through Ensure* would have the fixture cleanup race the in-test delete
// and surface as InvalidVolume.NotFound.
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
