package ocibsd_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/providers/cloud/oci"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs/ocibsd"
)

func TestNewRefusesANodeOutsideTheClustersAD(t *testing.T) {
	cfg := testConfig("node-1", oci.NewBlockFake(), ocibsd.NewFakeInitiator())
	cfg.InstanceAvailabilityDomain = "Uocm:US-ASHBURN-AD-2"
	_, err := ocibsd.New(cfg)
	require.ErrorContains(t, err, "AD-2")

	cfg = testConfig("node-1", oci.NewBlockFake(), ocibsd.NewFakeInitiator())
	cfg.CompartmentID = ""
	_, err = ocibsd.New(cfg)
	require.ErrorContains(t, err, "compartment ID is required")
}

func TestCapabilities(t *testing.T) {
	p := newCluster(t).nodes["node-1"]
	resp, err := p.GetCapabilities(context.Background(), ebsprovider.GetCapabilitiesRequest{Versioned: latest})
	require.NoError(t, err)
	caps := resp.Capabilities
	assert.Equal(t, ebsprovider.ExclusionSemantics{Scope: ebsprovider.ExclusionScopeCluster}, caps.Exclusion,
		"OCI refuses a second attachment anywhere, never lapses one, and does not fence a cut session")
	assert.False(t, caps.SnapshotCopy, "OCI copies a backup only to another region")
	assert.False(t, caps.VolumeSeeding)
	assert.False(t, caps.OnlineExpansion)
	assert.True(t, caps.ReadOnlyPublish)
	assert.True(t, caps.CrashConsistentSnapshot)
	assert.Equal(t, ebsprovider.SupportedSchemaVersions(), resp.ContractVersions)

	_, err = p.CopySnapshot(context.Background(), ebsprovider.CopySnapshotRequest{Versioned: latest, SourceSnapshotID: "snap-a", DestinationSnapshotID: "snap-b", VolumeID: "vol-a"})
	require.ErrorIs(t, err, ebsprovider.ErrUnsupportedCapability)
}

func TestCreateVolumeSizesAtTheFloorAndReportsTheRequest(t *testing.T) {
	cases := []struct {
		name     string
		capacity int64
		ociGB    int64
	}{
		{"below the floor", 1 << 30, oci.MinVolumeSizeGB},
		{"a part GB rounds up", 60<<30 + 1, 61},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCluster(t)
			p := c.router()
			v := createVolume(t, p, "vol-size", tc.capacity)
			assert.Equal(t, tc.capacity, v.CapacityBytes)
			assert.Equal(t, tc.ociGB, ociVolume(t, c.fake, v.Handle).SizeGB)

			got, err := p.GetVolume(context.Background(), ebsprovider.GetVolumeRequest{Versioned: latest, VolumeID: "vol-size"})
			require.NoError(t, err)
			assert.Equal(t, tc.capacity, got.CapacityBytes, "the requested size outlives the provider in a tag")

			pub, err := p.PublishVolume(context.Background(), ebsprovider.PublishVolumeRequest{Versioned: latest, VolumeID: "vol-size", NodeID: "node-1"})
			require.NoError(t, err)
			assert.Equal(t, uint64(tc.capacity), pub.Export.Block.SizeBytes, "the guest sees the requested size, not OCI's")
		})
	}
}

func TestCreateVolumeIsPlacedInTheClustersAD(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()

	v, err := c.nodes["node-2"].CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
		Versioned: latest, VolumeID: "vol-placed", AvailabilityZone: testAZ, CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
	})
	require.NoError(t, err)
	assert.Equal(t, testAD, ociVolume(t, c.fake, v.Handle).AvailabilityDomain)
	assert.Equal(t, testAZ, v.AvailabilityZone)

	_, err = c.nodes["node-1"].CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
		Versioned: latest, VolumeID: "vol-elsewhere", AvailabilityZone: "ap-southeast-2b", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
	})
	require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
	assert.Len(t, c.fake.Volumes(), 1, "no volume is created where no node can attach it")
}

func TestCreateVolumeMapsTheVolumeTypeToVPUs(t *testing.T) {
	cases := map[string]int64{"": 10, "gp2": 10, "gp3": 10, "st1": 10, "sc1": 10, "io1": 20, "io2": 20}
	for volumeType, vpus := range cases {
		t.Run(fmt.Sprintf("%q", volumeType), func(t *testing.T) {
			c := newCluster(t)
			v, err := c.router().CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{
				Versioned: latest, VolumeID: "vol-tier", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
				Parameters: ebsprovider.NewVolumeParameters(volumeType),
			})
			require.NoError(t, err)
			assert.Equal(t, vpus, ociVolume(t, c.fake, v.Handle).VPUsPerGB)
		})
	}

	c := newCluster(t)
	ctx := context.Background()
	_, err := c.router().CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
		Versioned: latest, VolumeID: "vol-magnetic", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
		Parameters: ebsprovider.NewVolumeParameters("standard"),
	})
	require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)

	createVolume(t, c.router(), "vol-retier", 1<<30)
	_, err = c.router().CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
		Versioned: latest, VolumeID: "vol-retier", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
		Parameters: ebsprovider.NewVolumeParameters("io1"),
	})
	require.ErrorIs(t, err, ebsprovider.ErrAlreadyExists, "a repeat asking for another tier is not the same volume")
}

func TestCreateVolumeAfterALostReplyReturnsTheFirstVolume(t *testing.T) {
	c := newCluster(t)
	p := c.router()
	ctx := context.Background()
	req := ebsprovider.CreateVolumeRequest{Versioned: latest, VolumeID: "vol-lost", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}}

	c.fake.LoseReply("CreateVolume")
	_, err := p.CreateVolume(ctx, req)
	require.ErrorIs(t, err, oci.ErrReplyLost)

	v, err := c.nodes["node-2"].CreateVolume(ctx, req)
	require.NoError(t, err, "the repeat may reach another worker")
	require.Len(t, c.fake.Volumes(), 1)
	assert.Equal(t, c.fake.Volumes()[0].ID, v.Handle)
	assert.Equal(t, ocibsd.RetryToken("volume", testCluster, "vol-lost"), c.fake.RetryToken(v.Handle))
}

func TestCreateVolumeRefusesARetryTokenReplayOfADeletedVolume(t *testing.T) {
	c := newCluster(t)
	p := c.router()
	ctx := context.Background()
	createVolume(t, p, "vol-again", 1<<30)
	require.NoError(t, p.DeleteVolume(ctx, ebsprovider.DeleteVolumeRequest{Versioned: latest, VolumeID: "vol-again"}))

	_, err := p.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: latest, VolumeID: "vol-again", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
	require.ErrorContains(t, err, "TERMINATED", "a terminated volume must never be answered as created")
}

func TestCreateVolumeFromASnapshot(t *testing.T) {
	c := newCluster(t)
	p := c.router()
	ctx := context.Background()
	createVolume(t, p, "vol-src", 70<<30)
	snap, err := p.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: latest, SnapshotID: "snap-src", VolumeID: "vol-src"})
	require.NoError(t, err)
	restore := ebsprovider.CreateVolumeRequest{
		Versioned: latest, VolumeID: "vol-restored", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 70 << 30},
		SourceSnapshotID: "snap-src", SourceSnapshotVolumeID: "vol-src",
	}

	_, err = p.CreateVolume(ctx, restore)
	require.ErrorIs(t, err, ebsprovider.ErrUnavailable, "a backup still being taken cannot be restored yet")

	require.NoError(t, c.fake.SettleBackup(snap.Handle, oci.BackupAvailable))
	small := restore
	small.CapacityRange.RequiredBytes = 1 << 30
	_, err = p.CreateVolume(ctx, small)
	require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument, "a volume smaller than its snapshot is refused")

	v, err := p.CreateVolume(ctx, restore)
	require.NoError(t, err)
	restored := ociVolume(t, c.fake, v.Handle)
	assert.Equal(t, snap.Handle, restored.SourceBackupID)
	assert.Equal(t, int64(70), restored.SizeGB)

	again, err := p.CreateVolume(ctx, restore)
	require.NoError(t, err)
	assert.Equal(t, v.Handle, again.Handle)
	blank := restore
	blank.SourceSnapshotID, blank.SourceSnapshotVolumeID = "", ""
	_, err = p.CreateVolume(ctx, blank)
	require.ErrorIs(t, err, ebsprovider.ErrAlreadyExists, "a blank create is not a repeat of a restore")
}

func TestCreateVolumeFromAFailedSnapshotIsRefused(t *testing.T) {
	c := newCluster(t)
	p := c.router()
	ctx := context.Background()
	createVolume(t, p, "vol-src", 1<<30)
	snap, err := p.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: latest, SnapshotID: "snap-faulty", VolumeID: "vol-src"})
	require.NoError(t, err)
	require.NoError(t, c.fake.SettleBackup(snap.Handle, oci.BackupFaulty))

	_, err = p.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
		Versioned: latest, VolumeID: "vol-from-faulty", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
		SourceSnapshotID: "snap-faulty", SourceSnapshotVolumeID: "vol-src",
	})
	require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
}

func TestAVolumeOfAnotherClusterIsNotFound(t *testing.T) {
	fake := oci.NewBlockFake()
	other := testConfig("node-1", fake, ocibsd.NewFakeInitiator())
	other.ClusterID = "cluster-2"
	theirs, err := ocibsd.New(other)
	require.NoError(t, err)
	v := createVolume(t, theirs, "vol-shared-name", 1<<30)

	ours := newClusterOver(t, fake, nil).router()
	ctx := context.Background()
	_, err = ours.GetVolume(ctx, ebsprovider.GetVolumeRequest{Versioned: latest, VolumeID: "vol-shared-name"})
	require.ErrorIs(t, err, ebsprovider.ErrNotFound)
	_, err = ours.GetVolume(ctx, ebsprovider.GetVolumeRequest{Versioned: latest, VolumeID: "vol-shared-name", Handle: v.Handle})
	require.ErrorIs(t, err, ebsprovider.ErrNotFound, "a handle is honoured only for this cluster's volume")
	require.NoError(t, ours.DeleteVolume(ctx, ebsprovider.DeleteVolumeRequest{Versioned: latest, VolumeID: "vol-shared-name", Handle: v.Handle}))
	assert.Equal(t, oci.VolumeAvailable, ociVolume(t, fake, v.Handle).State, "another cluster's volume is never deleted")
}

func TestExpandVolume(t *testing.T) {
	c := newCluster(t)
	p := c.router()
	ctx := context.Background()
	v := createVolume(t, p, "vol-grow", 1<<30)

	grown, err := p.ExpandVolume(ctx, ebsprovider.ExpandVolumeRequest{Versioned: latest, VolumeID: "vol-grow", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 2 << 30}})
	require.NoError(t, err)
	assert.Equal(t, int64(2<<30), grown.CapacityBytes)
	assert.Equal(t, int64(oci.MinVolumeSizeGB), ociVolume(t, c.fake, v.Handle).SizeGB, "within the floor only the recorded size changes")

	grown, err = p.ExpandVolume(ctx, ebsprovider.ExpandVolumeRequest{Versioned: latest, VolumeID: "vol-grow", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 80 << 30}})
	require.NoError(t, err)
	assert.Equal(t, int64(80<<30), grown.CapacityBytes)
	assert.Equal(t, int64(80), ociVolume(t, c.fake, v.Handle).SizeGB)

	same, err := p.ExpandVolume(ctx, ebsprovider.ExpandVolumeRequest{Versioned: latest, VolumeID: "vol-grow", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 80 << 30}})
	require.NoError(t, err)
	assert.Equal(t, int64(80<<30), same.CapacityBytes)

	_, err = p.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: latest, VolumeID: "vol-grow", NodeID: "node-1"})
	require.NoError(t, err)
	_, err = p.ExpandVolume(ctx, ebsprovider.ExpandVolumeRequest{Versioned: latest, VolumeID: "vol-grow", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 90 << 30}})
	require.ErrorIs(t, err, ebsprovider.ErrVolumeInUse)
}

func TestDeleteVolumeWhileABackupIsTakenIsInUse(t *testing.T) {
	c := newCluster(t)
	p := c.router()
	ctx := context.Background()
	createVolume(t, p, "vol-backing-up", 1<<30)
	_, err := p.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: latest, SnapshotID: "snap-taking", VolumeID: "vol-backing-up"})
	require.NoError(t, err)

	err = p.DeleteVolume(ctx, ebsprovider.DeleteVolumeRequest{Versioned: latest, VolumeID: "vol-backing-up"})
	require.ErrorIs(t, err, ebsprovider.ErrVolumeInUse, "OCI's 409 while a backup is taken")
	require.NoError(t, p.DeleteVolume(ctx, ebsprovider.DeleteVolumeRequest{Versioned: latest, VolumeID: "vol-never"}))
}

func TestSnapshots(t *testing.T) {
	c := newCluster(t)
	p := c.router()
	ctx := context.Background()
	createVolume(t, p, "vol-snap", 3<<30)
	createVolume(t, p, "vol-other", 1<<30)

	c.fake.LoseReply("CreateVolumeBackup")
	_, err := p.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: latest, SnapshotID: "snap-1", VolumeID: "vol-snap"})
	require.ErrorIs(t, err, oci.ErrReplyLost)
	snap, err := c.nodes["node-2"].CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: latest, SnapshotID: "snap-1", VolumeID: "vol-snap"})
	require.NoError(t, err)
	require.Len(t, c.fake.Backups(), 1, "a repeat after a lost reply takes no second backup")
	assert.Equal(t, ebsprovider.SnapshotStatePending, snap.State, "CreateSnapshot answers once OCI accepts the backup")
	assert.Equal(t, int64(3<<30), snap.SizeBytes)
	assert.Equal(t, "vol-snap", snap.SourceVolumeID)
	assert.Equal(t, ocibsd.RetryToken("snapshot", testCluster, "snap-1"), c.fake.RetryToken(snap.Handle))
	backup := c.fake.Backups()[0]
	assert.Equal(t, map[string]string{
		ocibsd.TagCluster: testCluster, ocibsd.TagSnapshotID: "snap-1", ocibsd.TagVolumeID: "vol-snap", ocibsd.TagCapacityBytes: fmt.Sprint(3 << 30),
	}, backup.FreeformTags)

	_, err = p.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: latest, SnapshotID: "snap-1", VolumeID: "vol-other"})
	require.ErrorIs(t, err, ebsprovider.ErrAlreadyExists)

	err = p.DeleteSnapshot(ctx, ebsprovider.DeleteSnapshotRequest{Versioned: latest, SnapshotID: "snap-1"})
	require.ErrorIs(t, err, ebsprovider.ErrUnavailable, "OCI's 409 on a backup still being taken is retry-later")

	read := func() ebsprovider.SnapshotState {
		resp, err := p.ListSnapshots(ctx, ebsprovider.ListSnapshotsRequest{Versioned: latest, SnapshotID: "snap-1"})
		require.NoError(t, err)
		require.Len(t, resp.Snapshots, 1)
		return resp.Snapshots[0].State
	}
	assert.Equal(t, ebsprovider.SnapshotStatePending, read(), "CREATING")
	require.NoError(t, c.fake.SettleBackup(snap.Handle, oci.BackupRequestReceived))
	assert.Equal(t, ebsprovider.SnapshotStatePending, read(), "REQUEST_RECEIVED")
	require.NoError(t, c.fake.SettleBackup(snap.Handle, oci.BackupAvailable))
	assert.Equal(t, ebsprovider.SnapshotStateCompleted, read(), "AVAILABLE")

	require.NoError(t, p.DeleteSnapshot(ctx, ebsprovider.DeleteSnapshotRequest{Versioned: latest, SnapshotID: "snap-1"}))
	require.NoError(t, p.DeleteSnapshot(ctx, ebsprovider.DeleteSnapshotRequest{Versioned: latest, SnapshotID: "snap-1"}), "a deleted snapshot deletes again")

	faulty, err := p.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: latest, SnapshotID: "snap-2", VolumeID: "vol-snap"})
	require.NoError(t, err)
	require.NoError(t, c.fake.SettleBackup(faulty.Handle, oci.BackupFaulty))
	resp, err := p.ListSnapshots(ctx, ebsprovider.ListSnapshotsRequest{Versioned: latest})
	require.NoError(t, err)
	require.Len(t, resp.Snapshots, 1, "a deleted backup is not enumerated")
	assert.Equal(t, ebsprovider.SnapshotStateError, resp.Snapshots[0].State, "FAULTY")
}

func TestPublishRepublishesOnlyInTheSameAccessMode(t *testing.T) {
	c := newCluster(t)
	p := c.router()
	ctx := context.Background()
	createVolume(t, p, "vol-pub", 1<<30)

	first, err := p.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: latest, VolumeID: "vol-pub", NodeID: "node-1"})
	require.NoError(t, err)
	require.NoError(t, first.Export.Validate())
	require.Equal(t, ebsprovider.ExportKindBlock, first.Export.Kind)
	sessions := c.initiators["node-1"].Sessions()
	require.Len(t, sessions, 1)
	assert.Equal(t, ocibsd.ByPathDevice(sessions[0]), first.Export.Block.DevicePath)
	assert.Regexp(t, `^/dev/disk/by-path/ip-169\.254\.2\.\d+:3260-iscsi-iqn\.2015-12\.com\.oracleiaas:.+-lun-1$`, first.Export.Block.DevicePath)

	again, err := p.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: latest, VolumeID: "vol-pub", NodeID: "node-1"})
	require.NoError(t, err)
	assert.Equal(t, first, again)
	attachments, err := c.fake.ListVolumeAttachments(ctx, oci.AttachmentFilter{InstanceID: instanceOf("node-1")})
	require.NoError(t, err)
	assert.Len(t, attachments, 1, "a republish reuses the attachment")

	_, err = p.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: latest, VolumeID: "vol-pub", NodeID: "node-1", ReadOnly: true})
	require.ErrorIs(t, err, ebsprovider.ErrVolumeInUse)
	assert.NotErrorIs(t, err, ebsprovider.ErrVolumeHeldElsewhere)
}

func TestPublishIsRefusedWhileAnotherInstanceHoldsTheVolume(t *testing.T) {
	c := newCluster(t)
	p := c.router()
	ctx := context.Background()
	v := createVolume(t, p, "vol-held", 1<<30)
	c.fake.SeedAttachment(oci.VolumeAttachment{ID: "ocid1.volumeattachment.oc1..seeded", InstanceID: "ocid1.instance.oc1..elsewhere", VolumeID: v.Handle, State: oci.AttachmentAttached})

	_, err := p.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: latest, VolumeID: "vol-held", NodeID: "node-1"})
	require.ErrorIs(t, err, ebsprovider.ErrVolumeHeldElsewhere)
	_, err = p.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-held", NodeID: "node-1"})
	require.ErrorIs(t, err, ebsprovider.ErrVolumeInUse)
	assert.NotErrorIs(t, err, ebsprovider.ErrVolumeHeldElsewhere, "a v1 caller predates held elsewhere")
}

// racingClient attaches the volume to another instance just before this
// node's attach reaches OCI, as a publish racing on another node does.
type racingClient struct{ *oci.BlockFake }

func (r racingClient) AttachVolume(ctx context.Context, in oci.AttachVolumeInput) (oci.VolumeAttachment, error) {
	r.SeedAttachment(oci.VolumeAttachment{ID: "ocid1.volumeattachment.oc1..raced", InstanceID: "ocid1.instance.oc1..racer", VolumeID: in.VolumeID, State: oci.AttachmentAttaching})
	return r.BlockFake.AttachVolume(ctx, in)
}

func TestPublishThatLosesAnAttachRaceIsHeldElsewhere(t *testing.T) {
	fake := oci.NewBlockFake()
	c := newClusterOver(t, fake, racingClient{fake})
	createVolume(t, c.router(), "vol-raced", 1<<30)

	_, err := c.router().PublishVolume(context.Background(), ebsprovider.PublishVolumeRequest{Versioned: latest, VolumeID: "vol-raced", NodeID: "node-1"})
	require.ErrorIs(t, err, ebsprovider.ErrVolumeHeldElsewhere)
}

func TestPublishRetryLaterRefusals(t *testing.T) {
	c := newCluster(t)
	p := c.router()
	ctx := context.Background()
	createVolume(t, p, "vol-a", 1<<30)
	createVolume(t, p, "vol-b", 1<<30)

	c.fake.FailOp("AttachVolume", fmt.Errorf("staged: %w", oci.ErrConflict))
	_, err := p.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: latest, VolumeID: "vol-a", NodeID: "node-1"})
	require.ErrorIs(t, err, ebsprovider.ErrUnavailable, "a conflict no attachment explains is retry-later")

	c.initiators["node-1"].FailNextLogin(errors.New("no session"))
	_, err = p.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: latest, VolumeID: "vol-a", NodeID: "node-1"})
	require.ErrorIs(t, err, ebsprovider.ErrUnavailable)
	_, err = p.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: latest, VolumeID: "vol-a", NodeID: "node-1"})
	require.NoError(t, err, "the retry logs in over the attachment the failed login left")

	c.fake.AttachmentCap = 1
	_, err = p.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: latest, VolumeID: "vol-b", NodeID: "node-1"})
	require.ErrorIs(t, err, ebsprovider.ErrUnavailable, "past the attachment cap a publish waits for a detach")
	_, err = p.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: latest, VolumeID: "vol-b", NodeID: "node-2"})
	require.NoError(t, err, "the cap is per instance")
}

func TestPublishOfADetachingAttachmentIsRetryLater(t *testing.T) {
	c := newCluster(t)
	p := c.router()
	v := createVolume(t, p, "vol-detaching", 1<<30)
	c.fake.SeedAttachment(oci.VolumeAttachment{ID: "ocid1.volumeattachment.oc1..leaving", InstanceID: instanceOf("node-1"), VolumeID: v.Handle, State: oci.AttachmentDetaching})

	_, err := p.PublishVolume(context.Background(), ebsprovider.PublishVolumeRequest{Versioned: latest, VolumeID: "vol-detaching", NodeID: "node-1"})
	require.ErrorIs(t, err, ebsprovider.ErrUnavailable)
}

func TestPerNodeVerbsRefuseAnotherNode(t *testing.T) {
	p := newCluster(t).nodes["node-1"]
	ctx := context.Background()
	_, err := p.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: latest, VolumeID: "vol-x", NodeID: "node-2"})
	require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
	_, err = p.UnpublishVolume(ctx, ebsprovider.UnpublishVolumeRequest{Versioned: latest, VolumeID: "vol-x", NodeID: "node-2"})
	require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
}

func TestUnpublish(t *testing.T) {
	c := newCluster(t)
	p := c.router()
	ctx := context.Background()
	v := createVolume(t, p, "vol-unpub", 1<<30)

	result, err := p.UnpublishVolume(ctx, ebsprovider.UnpublishVolumeRequest{Versioned: latest, VolumeID: "vol-unpub", NodeID: "node-1"})
	require.NoError(t, err, "a volume with no attachment here is already unpublished")
	assert.False(t, result.TornDown)
	result, err = p.UnpublishVolume(ctx, ebsprovider.UnpublishVolumeRequest{Versioned: latest, VolumeID: "vol-absent", NodeID: "node-1"})
	require.NoError(t, err)
	assert.False(t, result.TornDown)

	_, err = p.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: latest, VolumeID: "vol-unpub", NodeID: "node-1"})
	require.NoError(t, err)
	result, err = p.UnpublishVolume(ctx, ebsprovider.UnpublishVolumeRequest{Versioned: latest, VolumeID: "vol-unpub", NodeID: "node-2"})
	require.NoError(t, err, "node-2 holds no attachment of it")
	assert.False(t, result.TornDown)

	result, err = p.UnpublishVolume(ctx, ebsprovider.UnpublishVolumeRequest{Versioned: latest, VolumeID: "vol-unpub", NodeID: "node-1"})
	require.NoError(t, err)
	assert.True(t, result.TornDown)
	assert.Empty(t, c.initiators["node-1"].Sessions(), "the session is logged out")
	attachments, err := c.fake.ListVolumeAttachments(ctx, oci.AttachmentFilter{VolumeID: v.Handle})
	require.NoError(t, err)
	require.Len(t, attachments, 1)
	assert.Equal(t, oci.AttachmentDetached, attachments[0].State, "unpublish returns only once OCI reports DETACHED")
}

func TestUnpublishFinishesADetachAlreadyUnderWay(t *testing.T) {
	c := newCluster(t)
	p := c.router()
	v := createVolume(t, p, "vol-half", 1<<30)
	c.fake.SeedAttachment(oci.VolumeAttachment{ID: "ocid1.volumeattachment.oc1..half", InstanceID: instanceOf("node-1"), VolumeID: v.Handle, State: oci.AttachmentDetaching})

	result, err := p.UnpublishVolume(context.Background(), ebsprovider.UnpublishVolumeRequest{Versioned: latest, VolumeID: "vol-half", NodeID: "node-1"})
	require.NoError(t, err)
	assert.True(t, result.TornDown)
}

func TestHoldsUnsealedWritesAnswersNone(t *testing.T) {
	p := newCluster(t).nodes["node-1"]
	held, err := p.HoldsUnsealedWrites(context.Background(), "node-1", []string{"vol-a"})
	require.NoError(t, err)
	assert.Empty(t, held)
	_, err = p.HoldsUnsealedWrites(context.Background(), "node-1", []string{""})
	require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
}

// provisioningClient reports a volume PROVISIONING for its first reads, as
// OCI does for the seconds after a create.
type provisioningClient struct {
	*oci.BlockFake
	reads *int
	state oci.VolumeState
}

func (p provisioningClient) GetVolume(ctx context.Context, volumeID string) (oci.BlockVolume, error) {
	v, err := p.BlockFake.GetVolume(ctx, volumeID)
	if *p.reads > 0 && err == nil {
		*p.reads--
		v.State = p.state
	}
	return v, err
}

func TestCreateVolumeWaitsForAvailable(t *testing.T) {
	fake := oci.NewBlockFake()
	reads := 0
	c := newClusterOver(t, fake, provisioningClient{BlockFake: fake, reads: &reads, state: oci.VolumeProvisioning})
	p := c.router()
	ctx := context.Background()

	reads = 3
	v := createVolume(t, p, "vol-slow", 1<<30)
	assert.Equal(t, ebsprovider.VolumeStateAvailable, v.State)
	assert.Zero(t, reads, "the create read the volume until it was available")

	reads = 1
	got, err := p.GetVolume(ctx, ebsprovider.GetVolumeRequest{Versioned: latest, VolumeID: "vol-slow", Handle: v.Handle})
	require.NoError(t, err)
	assert.Equal(t, ebsprovider.VolumeStateCreating, got.State)

	faulty := newClusterOver(t, fake, provisioningClient{BlockFake: fake, reads: &reads, state: oci.VolumeFaulty})
	reads = 1
	_, err = faulty.router().CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: latest, VolumeID: "vol-faulty", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
	require.ErrorContains(t, err, "FAULTY")
}

func TestOCIQuotaRefusalsAreRetryLater(t *testing.T) {
	c := newCluster(t)
	c.fake.FailOp("CreateVolume", fmt.Errorf("staged: %w", oci.ErrLimitExceeded))
	_, err := c.router().CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{Versioned: latest, VolumeID: "vol-quota", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
	require.ErrorIs(t, err, ebsprovider.ErrUnavailable)
}

func TestAttachmentsThatDoNotSettleAreRetryLater(t *testing.T) {
	c := newCluster(t)
	p := c.router()
	ctx := context.Background()
	createVolume(t, p, "vol-stuck", 1<<30)

	c.fake.AttachAfter = 1 << 20
	_, err := p.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: latest, VolumeID: "vol-stuck", NodeID: "node-1"})
	require.ErrorIs(t, err, ebsprovider.ErrUnavailable, "an attachment still ATTACHING at the timeout")

	c.fake.DetachAfter = 1 << 20
	_, err = p.UnpublishVolume(ctx, ebsprovider.UnpublishVolumeRequest{Versioned: latest, VolumeID: "vol-stuck", NodeID: "node-1"})
	require.ErrorIs(t, err, ebsprovider.ErrUnavailable, "an attachment still DETACHING at the timeout")
}
