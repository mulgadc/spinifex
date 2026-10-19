package image_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ebsmetadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	ec2image "github.com/mulgadc/spinifex/spinifex/domains/ec2/image"
	ec2snapshot "github.com/mulgadc/spinifex/spinifex/domains/ec2/snapshot"
	ec2volume "github.com/mulgadc/spinifex/spinifex/domains/ec2/volume"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	ebsprovider "github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The TestCurrentBehaviour_AccountTermination_ tests pin how volumes,
// snapshots and images guard one another today, wired the way the daemon wires
// them; a failure means a dependency rule changed.

const (
	ctAccountID = "123456789012"
	ctBucket    = "ct-bucket"
	ctAZ        = "ct-az"
)

type storageServices struct {
	volumes   *ec2volume.VolumeServiceImpl
	snapshots *ec2snapshot.SnapshotServiceImpl
	images    *ec2image.ImageServiceImpl
	provider  *ebsprovider.MemoryProvider
	metadata  *ebsmetadata.Store
}

// newStorageServices shares one object store, one provider and one
// volume-to-snapshot index between the three services. As in the daemon, the
// image service is given no handle on that index.
func newStorageServices(t *testing.T) storageServices {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)
	refs, err := js.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{Bucket: ec2snapshot.KVBucketVolumeSnapshots})
	require.NoError(t, err)

	store := objectstore.NewMemoryObjectStore()
	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{SnapshotEnumeration: true})
	cfg := &config.Config{AZ: ctAZ, WalDir: t.TempDir(), Predastore: config.PredastoreConfig{Bucket: ctBucket}}

	volumes := ec2volume.NewVolumeServiceImplWithStore(cfg, store, nc, refs)
	volumes.SetEBSProvider(provider)
	snapshots := ec2snapshot.NewSnapshotServiceImplWithStore(cfg, store, nc, refs)
	snapshots.SetEBSProvider(provider)
	images := ec2image.NewImageServiceImplWithConfig(cfg, store, nil)
	images.SetEBSProvider(provider)

	return storageServices{
		volumes: volumes, snapshots: snapshots, images: images,
		provider: provider, metadata: ebsmetadata.NewStore(store, ctBucket),
	}
}

func (s storageServices) seedVolume(t *testing.T, volumeID string) {
	t.Helper()
	ctx := context.Background()
	created, err := s.provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
		Versioned:     ebsprovider.NewVersioned(),
		VolumeID:      volumeID,
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
	})
	require.NoError(t, err)
	require.NoError(t, s.metadata.PutVolume(ctx, ebsmetadata.Volume{
		VolumeID: volumeID, TenantID: ctAccountID, CapacityGiB: 1,
		State: "available", ProviderHandle: created.Handle,
	}))
}

func (s storageServices) createImage(t *testing.T, rootVolumeID, name string) string {
	t.Helper()
	out, err := s.images.CreateImageFromInstance(ec2image.CreateImageParams{
		Input:        &ec2.CreateImageInput{InstanceId: aws.String("i-0123456789abcdef0"), Name: aws.String(name)},
		RootVolumeID: rootVolumeID,
	}, ctAccountID)
	require.NoError(t, err)
	return aws.StringValue(out.ImageId)
}

func (s storageServices) deleteVolume(volumeID string) error {
	_, err := s.volumes.DeleteVolume(context.Background(), &ec2.DeleteVolumeInput{VolumeId: aws.String(volumeID)}, ctAccountID)
	return err
}

// A CreateSnapshot snapshot pins its source volume through the index, but a
// CreateImage snapshot is never added to it, so its source volume deletes and
// the image keeps naming a volume that no longer exists.
func TestCurrentBehaviour_AccountTermination_CreateImageSnapshotDoesNotPinSourceVolume(t *testing.T) {
	s := newStorageServices(t)
	ctx := context.Background()

	s.seedVolume(t, "vol-snapshotted")
	_, err := s.snapshots.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{VolumeId: aws.String("vol-snapshotted")}, ctAccountID)
	require.NoError(t, err)
	assert.EqualError(t, s.deleteVolume("vol-snapshotted"), awserrors.ErrorVolumeInUse,
		"a CreateSnapshot snapshot blocks deleting its source volume")

	s.seedVolume(t, "vol-imaged")
	imageID := s.createImage(t, "vol-imaged", "ct-image")
	require.NoError(t, s.deleteVolume("vol-imaged"), "a CreateImage snapshot does not block deleting its source volume")

	_, err = s.metadata.GetVolume(ctx, ctAccountID, "vol-imaged")
	require.Error(t, err, "the source volume's document is gone")

	images, err := s.images.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []*string{aws.String(imageID)}}, ctAccountID)
	require.NoError(t, err)
	require.Len(t, images.Images, 1)
	assert.Equal(t, ec2.ImageStateAvailable, aws.StringValue(images.Images[0].State), "the image still reports available")

	source, err := s.images.GetAMISourceVolumeID(ctx, imageID)
	require.NoError(t, err)
	assert.Equal(t, "vol-imaged", source, "a launch from the image still resolves the deleted volume")
}

// DeleteSnapshot consults volumes cloned from the snapshot, not images
// registered on it, so the snapshot under a live image deletes.
func TestCurrentBehaviour_AccountTermination_DeleteSnapshotIgnoresRegisteredImage(t *testing.T) {
	s := newStorageServices(t)
	ctx := context.Background()

	s.seedVolume(t, "vol-registered")
	snap, err := s.snapshots.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{VolumeId: aws.String("vol-registered")}, ctAccountID)
	require.NoError(t, err)
	snapshotID := aws.StringValue(snap.SnapshotId)

	registered, err := s.images.RegisterImage(ctx, &ec2.RegisterImageInput{
		Name:           aws.String("ct-registered"),
		RootDeviceName: aws.String("/dev/sda1"),
		BlockDeviceMappings: []*ec2.BlockDeviceMapping{{
			DeviceName: aws.String("/dev/sda1"),
			Ebs:        &ec2.EbsBlockDevice{SnapshotId: aws.String(snapshotID)},
		}},
	}, ctAccountID)
	require.NoError(t, err)

	_, err = s.snapshots.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: aws.String(snapshotID)}, ctAccountID)
	require.NoError(t, err, "the snapshot deletes while an image is registered on it")

	images, err := s.images.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []*string{registered.ImageId}}, ctAccountID)
	require.NoError(t, err)
	require.Len(t, images.Images, 1)
	assert.Equal(t, ec2.ImageStateAvailable, aws.StringValue(images.Images[0].State), "the image still reports available")

	_, err = s.images.GetAMISourceVolumeID(ctx, aws.StringValue(registered.ImageId))
	assert.EqualError(t, err, awserrors.ErrorInvalidSnapshotNotFound, "a launch from the image can no longer resolve its snapshot")
	require.NoError(t, s.deleteVolume("vol-registered"), "with the snapshot gone, the source volume deletes too")
}

// deleteDuringClone runs a hook once, after the provider has accepted a clone
// from a snapshot and before CreateVolume writes the clone's document.
type deleteDuringClone struct {
	ebsprovider.EBSProvider

	hook  func()
	fired bool
}

func (p *deleteDuringClone) CreateVolume(ctx context.Context, req ebsprovider.CreateVolumeRequest) (*ebsprovider.Volume, error) {
	created, err := p.EBSProvider.CreateVolume(ctx, req)
	if err == nil && req.SourceSnapshotID != "" && !p.fired {
		p.fired = true
		p.hook()
	}
	return created, err
}

// DeleteSnapshot's clone check reads volume documents, and CreateVolume writes
// the clone's document only after the provider has verified and cloned the
// snapshot, so a delete in that window passes the check and strands the clone.
func TestCurrentBehaviour_AccountTermination_DeleteSnapshotDuringCloneCreationStrandsTheClone(t *testing.T) {
	s := newStorageServices(t)
	ctx := context.Background()
	createFrom := func(snapshotID string) (*ec2.Volume, error) {
		return s.volumes.CreateVolume(ctx, &ec2.CreateVolumeInput{
			AvailabilityZone: aws.String(ctAZ), SnapshotId: aws.String(snapshotID),
		}, ctAccountID)
	}
	deleteSnapshot := func(snapshotID string) error {
		_, err := s.snapshots.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: aws.String(snapshotID)}, ctAccountID)
		return err
	}

	s.seedVolume(t, "vol-settled")
	settled, err := s.snapshots.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{VolumeId: aws.String("vol-settled")}, ctAccountID)
	require.NoError(t, err)
	_, err = createFrom(aws.StringValue(settled.SnapshotId))
	require.NoError(t, err)
	assert.EqualError(t, deleteSnapshot(aws.StringValue(settled.SnapshotId)), awserrors.ErrorInvalidSnapshotInUse,
		"once the clone's document exists, the delete is refused")

	s.seedVolume(t, "vol-raced")
	raced, err := s.snapshots.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{VolumeId: aws.String("vol-raced")}, ctAccountID)
	require.NoError(t, err)
	snapshotID := aws.StringValue(raced.SnapshotId)

	var raceErr error
	s.volumes.SetEBSProvider(&deleteDuringClone{EBSProvider: s.provider, hook: func() { raceErr = deleteSnapshot(snapshotID) }})
	clone, err := createFrom(snapshotID)
	require.NoError(t, err, "CreateVolume reports success")
	require.NoError(t, raceErr, "the delete inside the window passes the clone check")

	doc, err := s.metadata.GetVolume(ctx, ctAccountID, aws.StringValue(clone.VolumeId))
	require.NoError(t, err)
	assert.Equal(t, snapshotID, doc.SnapshotID, "the clone's document names the deleted snapshot")
	_, err = s.metadata.GetSnapshot(ctx, ctAccountID, snapshotID)
	require.Error(t, err, "the snapshot's document is gone")
	_, held := s.provider.Snapshot(snapshotID)
	assert.False(t, held, "the provider no longer holds the snapshot the clone was made from")

	_, err = createFrom(snapshotID)
	assert.EqualError(t, err, awserrors.ErrorInvalidSnapshotNotFound, "a clone started after the delete is refused")
}

// CreateImage on an instance re-reads the image the instance launched from,
// after the provider has taken the snapshot, so once that image is
// deregistered the call fails and leaves a provider snapshot with no document.
func TestCurrentBehaviour_AccountTermination_CreateImageOfRetainedInstanceNeedsItsLaunchImage(t *testing.T) {
	s := newStorageServices(t)
	ctx := context.Background()

	s.seedVolume(t, "vol-launch-source")
	launchImageID := s.createImage(t, "vol-launch-source", "ct-launch-image")
	_, err := s.images.DeregisterImage(ctx, &ec2.DeregisterImageInput{ImageId: aws.String(launchImageID)}, ctAccountID)
	require.NoError(t, err)

	s.seedVolume(t, "vol-retained-root")
	_, err = s.images.CreateImageFromInstance(ec2image.CreateImageParams{
		Input:         &ec2.CreateImageInput{InstanceId: aws.String("i-0123456789abcdef0"), Name: aws.String("ct-after-deregister")},
		RootVolumeID:  "vol-retained-root",
		SourceImageID: launchImageID,
	}, ctAccountID)
	assert.EqualError(t, err, awserrors.ErrorServerInternal, "the instance's image can no longer be created")

	listed, err := s.provider.ListSnapshots(ctx, ebsprovider.ListSnapshotsRequest{Versioned: ebsprovider.NewVersioned()})
	require.NoError(t, err)
	var orphans []string
	for _, ref := range listed.Snapshots {
		if ref.SourceVolumeID == "vol-retained-root" {
			orphans = append(orphans, ref.ID)
		}
	}
	require.Len(t, orphans, 1, "the provider kept the snapshot it took before the image read failed")

	docs, err := s.metadata.ListSnapshotsStrict(ctx, ctAccountID)
	require.NoError(t, err)
	for _, doc := range docs {
		assert.NotEqual(t, orphans[0], doc.SnapshotID, "no document names that snapshot")
	}
}
