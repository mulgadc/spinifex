package volume

//test:in-package — the creating state is asserted on the metadata document,
// and the metadata store is unexported.

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createPreparingVolume(t *testing.T, svc *VolumeServiceImpl) string {
	t.Helper()
	vol, err := svc.CreateVolumeForPreparation(context.Background(), &ec2.CreateVolumeInput{
		Size: aws.Int64(4), AvailabilityZone: aws.String("ap-southeast-2a"),
	}, testVolAccountID)
	require.NoError(t, err)
	assert.Equal(t, "creating", aws.StringValue(vol.State))
	return aws.StringValue(vol.VolumeId)
}

func requireVolumeState(t *testing.T, svc *VolumeServiceImpl, volumeID, state string) {
	t.Helper()
	meta, err := svc.metadata.GetVolume(context.Background(), testVolAccountID, volumeID)
	require.NoError(t, err)
	assert.Equal(t, state, meta.State)
	assert.Empty(t, meta.AttachedInstance)
	assert.Equal(t, uint64(4), meta.CapacityGiB)
}

func TestCreateVolumeForPreparation_IsBornCreatingWhileCreateVolumeStaysAvailable(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	id := createPreparingVolume(t, svc)
	requireVolumeState(t, svc, id, "creating")

	described, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{VolumeIds: []*string{aws.String(id)}}, testVolAccountID)
	require.NoError(t, err)
	require.Len(t, described.Volumes, 1)
	assert.Equal(t, "creating", aws.StringValue(described.Volumes[0].State))

	normal, err := svc.CreateVolume(context.Background(), &ec2.CreateVolumeInput{
		Size: aws.Int64(4), AvailabilityZone: aws.String("ap-southeast-2a"),
	}, testVolAccountID)
	require.NoError(t, err)
	assert.Equal(t, "available", aws.StringValue(normal.State))
	requireVolumeState(t, svc, aws.StringValue(normal.VolumeId), "available")
}

// Every public path that could take, change or capture the volume refuses it
// and leaves it exactly as it was until preparation completes.
func TestCreatingVolume_PublicMutationsAreRefused(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	svc.snapshotKV = setupTestVolumeKV(t)
	id := createPreparingVolume(t, svc)
	ctx := context.Background()

	_, err := svc.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: aws.String(id)}, testVolAccountID)
	assert.EqualError(t, err, awserrors.ErrorVolumeInUse)
	requireVolumeState(t, svc, id, "creating")

	_, err = svc.ModifyVolume(ctx, &ec2.ModifyVolumeInput{VolumeId: aws.String(id), Size: aws.Int64(8)}, testVolAccountID)
	assert.EqualError(t, err, awserrors.ErrorIncorrectState)
	requireVolumeState(t, svc, id, "creating")

	_, err = svc.ForceDetachVolume(ctx, &ec2.DetachVolumeInput{VolumeId: aws.String(id), Force: aws.Bool(true)}, testVolAccountID)
	assert.EqualError(t, err, awserrors.ErrorIncorrectState)
	requireVolumeState(t, svc, id, "creating")
}

func TestCompleteVolumePreparation_ReleasesTheVolume(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	svc.snapshotKV = setupTestVolumeKV(t)
	id := createPreparingVolume(t, svc)
	ctx := context.Background()

	require.NoError(t, svc.CompleteVolumePreparation(ctx, testVolAccountID, id))
	requireVolumeState(t, svc, id, "available")

	// Released means ordinary behavior again: modify and delete now proceed.
	_, err := svc.ModifyVolume(ctx, &ec2.ModifyVolumeInput{VolumeId: aws.String(id), Size: aws.Int64(8)}, testVolAccountID)
	require.NoError(t, err)
	_, err = svc.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: aws.String(id)}, testVolAccountID)
	require.NoError(t, err)
}

// Completion only releases an unattached creating volume, so it can never
// flip a volume some other path owns to available.
func TestCompleteVolumePreparation_RefusesAnythingButAnUnattachedCreatingVolume(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	ctx := context.Background()

	normal, err := svc.CreateVolume(ctx, &ec2.CreateVolumeInput{
		Size: aws.Int64(4), AvailabilityZone: aws.String("ap-southeast-2a"),
	}, testVolAccountID)
	require.NoError(t, err)
	err = svc.CompleteVolumePreparation(ctx, testVolAccountID, aws.StringValue(normal.VolumeId))
	require.ErrorContains(t, err, awserrors.ErrorIncorrectState)

	require.NoError(t, svc.UpdateVolumeState(testVolAccountID, aws.StringValue(normal.VolumeId), "in-use", "i-0123456789abcdef0", "/dev/sdf"))
	err = svc.CompleteVolumePreparation(ctx, testVolAccountID, aws.StringValue(normal.VolumeId))
	require.ErrorContains(t, err, awserrors.ErrorIncorrectState)
	meta, err := svc.metadata.GetVolume(ctx, testVolAccountID, aws.StringValue(normal.VolumeId))
	require.NoError(t, err)
	assert.Equal(t, "in-use", meta.State)

	id := createPreparingVolume(t, svc)
	err = svc.CompleteVolumePreparation(ctx, "000000000099", id)
	assert.EqualError(t, err, awserrors.ErrorInvalidVolumeNotFound)
	requireVolumeState(t, svc, id, "creating")

	err = svc.CompleteVolumePreparation(ctx, testVolAccountID, "vol-0000000000000dead")
	assert.EqualError(t, err, awserrors.ErrorInvalidVolumeNotFound)
}
