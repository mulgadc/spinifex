package oci_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/providers/cloud/oci"
)

func fakeVolume(t *testing.T, f *oci.BlockFake, name string) oci.BlockVolume {
	t.Helper()
	v, err := f.CreateVolume(context.Background(), oci.CreateVolumeInput{
		CompartmentID: "c1", AvailabilityDomain: "AD-1", DisplayName: name, SizeGB: oci.MinVolumeSizeGB,
		FreeformTags: map[string]string{"spinifex-volume-id": name},
	})
	require.NoError(t, err)
	return v
}

func TestBlockFakeVolumeFloorAndRetryToken(t *testing.T) {
	ctx := context.Background()
	f := oci.NewBlockFake()

	_, err := f.CreateVolume(ctx, oci.CreateVolumeInput{CompartmentID: "c1", AvailabilityDomain: "AD-1", SizeGB: oci.MinVolumeSizeGB - 1})
	require.Error(t, err, "OCI refuses a volume below its 50 GB floor")

	in := oci.CreateVolumeInput{CompartmentID: "c1", AvailabilityDomain: "AD-1", SizeGB: 60, RetryToken: "t1"}
	first, err := f.CreateVolume(ctx, in)
	require.NoError(t, err)
	assert.Equal(t, int64(10), first.VPUsPerGB, "an unset tier is OCI's Balanced default")
	again, err := f.CreateVolume(ctx, in)
	require.NoError(t, err)
	assert.Equal(t, first.ID, again.ID, "a repeated retry token answers with the first volume")
	assert.Equal(t, "t1", f.RetryToken(first.ID))
	assert.Len(t, f.Volumes(), 1)
}

func TestBlockFakeLostReplyStillTakesEffect(t *testing.T) {
	ctx := context.Background()
	f := oci.NewBlockFake()
	f.LoseReply("CreateVolume")

	_, err := f.CreateVolume(ctx, oci.CreateVolumeInput{CompartmentID: "c1", AvailabilityDomain: "AD-1", SizeGB: 50})
	require.ErrorIs(t, err, oci.ErrReplyLost)
	assert.Len(t, f.Volumes(), 1, "the volume exists although its creator never heard back")

	f.FailOp("GetVolume", fmt.Errorf("boom"))
	_, err = f.GetVolume(ctx, f.Volumes()[0].ID)
	require.EqualError(t, err, "boom")
	_, err = f.GetVolume(ctx, f.Volumes()[0].ID)
	require.NoError(t, err, "a staged failure is used once")
}

// The experiment's two refusals: OCI will not delete a volume or a backup
// while a backup of it is still being taken.
func TestBlockFakeBackupInProgressRefusesDeletes(t *testing.T) {
	ctx := context.Background()
	f := oci.NewBlockFake()
	v := fakeVolume(t, f, "vol-1")

	b, err := f.CreateVolumeBackup(ctx, oci.CreateVolumeBackupInput{VolumeID: v.ID, DisplayName: "snap-1", RetryToken: "t2"})
	require.NoError(t, err)
	assert.Equal(t, oci.BackupCreating, b.State)
	assert.Equal(t, v.SizeGB, b.SizeGB)
	assert.Equal(t, "c1", b.CompartmentID)

	require.ErrorIs(t, f.DeleteVolume(ctx, v.ID), oci.ErrConflict)
	require.ErrorIs(t, f.DeleteVolumeBackup(ctx, b.ID), oci.ErrConflict)
	_, err = f.CreateVolume(ctx, oci.CreateVolumeInput{CompartmentID: "c1", AvailabilityDomain: "AD-1", SizeGB: 50, SourceBackupID: b.ID})
	require.ErrorIs(t, err, oci.ErrConflict, "a backup still being taken cannot be restored")

	require.NoError(t, f.SettleBackup(b.ID, oci.BackupAvailable))
	require.Error(t, f.SettleBackup(b.ID, oci.BackupFaulty), "a settled backup does not settle again")
	require.Error(t, f.SettleBackup("absent", oci.BackupAvailable))

	restored, err := f.CreateVolume(ctx, oci.CreateVolumeInput{CompartmentID: "c1", AvailabilityDomain: "AD-1", SizeGB: 50, SourceBackupID: b.ID})
	require.NoError(t, err)
	assert.Equal(t, b.ID, restored.SourceBackupID)

	require.NoError(t, f.DeleteVolume(ctx, v.ID))
	got, err := f.GetVolume(ctx, v.ID)
	require.NoError(t, err, "OCI keeps a terminated volume readable")
	assert.Equal(t, oci.VolumeTerminated, got.State)
	require.ErrorIs(t, f.DeleteVolume(ctx, v.ID), oci.ErrNotFound)

	require.NoError(t, f.DeleteVolumeBackup(ctx, b.ID))
	require.ErrorIs(t, f.DeleteVolumeBackup(ctx, b.ID), oci.ErrNotFound)
	_, err = f.CreateVolume(ctx, oci.CreateVolumeInput{CompartmentID: "c1", AvailabilityDomain: "AD-1", SizeGB: 50, SourceBackupID: b.ID})
	require.ErrorIs(t, err, oci.ErrNotFound)
}

func TestBlockFakeAttachmentLifecycle(t *testing.T) {
	ctx := context.Background()
	f := oci.NewBlockFake()
	v := fakeVolume(t, f, "vol-1")

	a, err := f.AttachVolume(ctx, oci.AttachVolumeInput{InstanceID: "i1", VolumeID: v.ID})
	require.NoError(t, err)
	assert.Equal(t, oci.AttachmentAttaching, a.State)
	assert.NotEmpty(t, a.IQN)
	assert.Equal(t, 3260, a.Port)

	_, err = f.AttachVolume(ctx, oci.AttachVolumeInput{InstanceID: "i2", VolumeID: v.ID})
	require.ErrorIs(t, err, oci.ErrConflict, "a volume that is not shareable attaches once")
	require.ErrorIs(t, f.DeleteVolume(ctx, v.ID), oci.ErrConflict, "an attached volume cannot be deleted")

	got, err := f.GetVolumeAttachment(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, oci.AttachmentAttached, got.State)

	require.NoError(t, f.DetachVolume(ctx, a.ID))
	listed, err := f.ListVolumeAttachments(ctx, oci.AttachmentFilter{CompartmentID: "c1", VolumeID: v.ID})
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, oci.AttachmentDetaching, listed[0].State)
	require.ErrorIs(t, f.DetachVolume(ctx, a.ID), oci.ErrConflict)

	got, err = f.GetVolumeAttachment(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, oci.AttachmentDetached, got.State)
	none, err := f.ListVolumeAttachments(ctx, oci.AttachmentFilter{CompartmentID: "c1", InstanceID: "i2"})
	require.NoError(t, err)
	assert.Empty(t, none)

	_, err = f.AttachVolume(ctx, oci.AttachVolumeInput{InstanceID: "i2", VolumeID: v.ID})
	require.NoError(t, err, "a detached volume attaches again")
}

func TestBlockFakeAttachmentCap(t *testing.T) {
	ctx := context.Background()
	f := oci.NewBlockFake()
	f.AttachAfter = 0
	for i := range oci.MaxISCSIAttachmentsPerInstance {
		v := fakeVolume(t, f, fmt.Sprintf("vol-%d", i))
		_, err := f.AttachVolume(ctx, oci.AttachVolumeInput{InstanceID: "i1", VolumeID: v.ID})
		require.NoError(t, err)
	}
	extra := fakeVolume(t, f, "vol-extra")
	_, err := f.AttachVolume(ctx, oci.AttachVolumeInput{InstanceID: "i1", VolumeID: extra.ID})
	require.ErrorIs(t, err, oci.ErrLimitExceeded)
	_, err = f.AttachVolume(ctx, oci.AttachVolumeInput{InstanceID: "i2", VolumeID: extra.ID})
	require.NoError(t, err, "the cap is per instance")
}

func TestBlockFakeListsFilterByCompartmentNameAndTag(t *testing.T) {
	ctx := context.Background()
	f := oci.NewBlockFake()
	v := fakeVolume(t, f, "vol-1")
	fakeVolume(t, f, "vol-2")
	_, err := f.CreateVolumeBackup(ctx, oci.CreateVolumeBackupInput{VolumeID: v.ID, DisplayName: "snap-1", FreeformTags: map[string]string{"k": "a"}})
	require.NoError(t, err)

	vols, err := f.ListVolumes(ctx, oci.VolumeFilter{CompartmentID: "c1", Tags: map[string]string{"spinifex-volume-id": "vol-1"}})
	require.NoError(t, err)
	require.Len(t, vols, 1)
	assert.Equal(t, v.ID, vols[0].ID)
	vols, err = f.ListVolumes(ctx, oci.VolumeFilter{CompartmentID: "c1", DisplayName: "vol-2", AvailabilityDomain: "AD-1"})
	require.NoError(t, err)
	assert.Len(t, vols, 1)
	vols, err = f.ListVolumes(ctx, oci.VolumeFilter{CompartmentID: "c2"})
	require.NoError(t, err)
	assert.Empty(t, vols)

	backups, err := f.ListVolumeBackups(ctx, oci.BackupFilter{CompartmentID: "c1", Tags: map[string]string{"k": "a"}})
	require.NoError(t, err)
	assert.Len(t, backups, 1)
	backups, err = f.ListVolumeBackups(ctx, oci.BackupFilter{CompartmentID: "c1", DisplayName: "snap-2"})
	require.NoError(t, err)
	assert.Empty(t, backups)
}

func TestBlockFakeUpdateVolume(t *testing.T) {
	ctx := context.Background()
	f := oci.NewBlockFake()
	v := fakeVolume(t, f, "vol-1")

	got, err := f.UpdateVolume(ctx, v.ID, oci.UpdateVolumeInput{FreeformTags: map[string]string{"k": "v"}})
	require.NoError(t, err)
	assert.Equal(t, v.SizeGB, got.SizeGB)
	assert.Equal(t, map[string]string{"k": "v"}, got.FreeformTags)
	got, err = f.UpdateVolume(ctx, v.ID, oci.UpdateVolumeInput{SizeGB: 80})
	require.NoError(t, err)
	assert.Equal(t, int64(80), got.SizeGB)
	assert.Equal(t, map[string]string{"k": "v"}, got.FreeformTags, "a resize keeps the tags")
	_, err = f.UpdateVolume(ctx, v.ID, oci.UpdateVolumeInput{SizeGB: 60})
	require.Error(t, err, "a volume cannot shrink")
	_, err = f.UpdateVolume(ctx, "absent", oci.UpdateVolumeInput{SizeGB: 60})
	require.ErrorIs(t, err, oci.ErrNotFound)
}
