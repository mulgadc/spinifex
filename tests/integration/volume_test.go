//go:build integration

package integration

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCreateVolume_PersistsToRealPredastore is the storage-touching proof
// case for the real-predastore fixture (StartVolumeDaemonLite): it drives
// ec2.CreateVolume through the real gateway and a real VolumeServiceImpl,
// which constructs viperblock.New and calls Backend.Init()/SaveState()
// against an actual predastore daemon rather than an unreachable host. The
// previous unit-test equivalent (handlers/ec2/volume's
// TestCreateVolume_PassesValidation) could only assert the returned error
// wasn't a validation error, because nothing past validation could execute
// without a live backend. Here CreateVolume is expected to fully succeed,
// and DescribeVolumes reloads the volume from that same real backend
// (config.json read back over S3) rather than from any in-memory state the
// handler might be holding, so this proves the volume was actually
// persisted.
func TestCreateVolume_PersistsToRealPredastore(t *testing.T) {
	gw := StartGateway(t)
	StartVolumeDaemonLite(t, gw)

	client := gw.EC2Client(t)

	out, err := client.CreateVolume(&ec2.CreateVolumeInput{
		Size:             aws.Int64(8),
		AvailabilityZone: aws.String(testAZ),
	})
	require.NoError(t, err, "CreateVolume")
	require.NotNil(t, out.VolumeId)

	desc, err := client.DescribeVolumes(&ec2.DescribeVolumesInput{
		VolumeIds: []*string{out.VolumeId},
	})
	require.NoError(t, err, "DescribeVolumes")
	require.Len(t, desc.Volumes, 1, "volume should reload from the real predastore backend")
	assert.Equal(t, aws.StringValue(out.VolumeId), aws.StringValue(desc.Volumes[0].VolumeId))
	assert.Equal(t, int64(8), aws.Int64Value(desc.Volumes[0].Size))
	assert.Equal(t, "gp3", aws.StringValue(desc.Volumes[0].VolumeType))
	assert.Equal(t, "available", aws.StringValue(desc.Volumes[0].State))
}
