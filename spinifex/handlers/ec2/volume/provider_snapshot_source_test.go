package handlers_ec2_volume

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	handlers_ec2_instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	spxtypes "github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const providerTestAZ = "ap-southeast-2a"

// snapshotSourceProvider mirrors viperblockd's handleCreateVolume validation:
// a clone resolves its base blocks against the source volume's prefix, so a
// snapshot source without a source volume is rejected outright.
type snapshotSourceProvider struct {
	*ebsprovider.MemoryProvider

	lastRequest ebsprovider.CreateVolumeRequest
}

func (p *snapshotSourceProvider) CreateVolume(ctx context.Context, req ebsprovider.CreateVolumeRequest) (*ebsprovider.Volume, error) {
	p.lastRequest = req
	if req.SourceSnapshotID != "" && req.SourceSnapshotVolumeID == "" {
		return nil, fmt.Errorf("%w: source_snapshot_volume_id is required with source_snapshot_id", ebsprovider.ErrInvalidArgument)
	}
	// The snapshot exists only in the control plane's metadata for this test,
	// so bypass the in-memory provider's own snapshot bookkeeping.
	req.SourceSnapshotID = ""
	return p.MemoryProvider.CreateVolume(ctx, req)
}

func newSnapshotSourceProvider() *snapshotSourceProvider {
	return &snapshotSourceProvider{MemoryProvider: ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})}
}

// writeSnapshotMetadata publishes the control-plane record CreateVolume reads
// to learn which volume a snapshot was taken from. The owner must be set:
// CreateVolume fails closed on a snapshot whose owner it cannot match.
func writeSnapshotMetadata(t *testing.T, store objectstore.ObjectStore, snapshotID, sourceVolumeID, ownerID string, sizeGiB int64) {
	t.Helper()
	data, err := json.Marshal(snapshotMetadata{VolumeID: sourceVolumeID, VolumeSize: sizeGiB, OwnerID: ownerID})
	require.NoError(t, err)
	_, err = store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(snapshotID + "/metadata.json"),
		Body:   strings.NewReader(string(data)),
	})
	require.NoError(t, err)
}

// TestCreateVolume_Provider_FromSnapshotSendsSourceVolumeID locks that a
// snapshot-backed CreateVolume names the snapshot's source volume. Without it
// the provider rejects the request and create-from-snapshot cannot work at all.
func TestCreateVolume_Provider_FromSnapshotSendsSourceVolumeID(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	cfg := &config.Config{AZ: providerTestAZ, Predastore: config.PredastoreConfig{Bucket: "test-bucket"}}
	svc := NewVolumeServiceImplWithStore(cfg, store, nil)
	provider := newSnapshotSourceProvider()
	svc.SetEBSProvider(provider)

	writeSnapshotMetadata(t, store, "snap-src", "vol-origin", "acct-1", 8)

	created, err := svc.CreateVolume(context.Background(), &ec2.CreateVolumeInput{
		Size: aws.Int64(8), AvailabilityZone: aws.String(providerTestAZ), SnapshotId: aws.String("snap-src"),
	}, "acct-1")
	require.NoError(t, err)
	require.NotNil(t, created)

	assert.Equal(t, "snap-src", provider.lastRequest.SourceSnapshotID)
	assert.Equal(t, "vol-origin", provider.lastRequest.SourceSnapshotVolumeID,
		"the snapshot's source volume must travel with the snapshot ID")
}

// TestCreateVolume_Provider_WithoutSnapshotSendsNoSource locks that an ordinary
// blank volume does not gain a spurious source volume.
func TestCreateVolume_Provider_WithoutSnapshotSendsNoSource(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	cfg := &config.Config{AZ: providerTestAZ, Predastore: config.PredastoreConfig{Bucket: "test-bucket"}}
	svc := NewVolumeServiceImplWithStore(cfg, store, nil)
	provider := newSnapshotSourceProvider()
	svc.SetEBSProvider(provider)

	_, err := svc.CreateVolume(context.Background(), &ec2.CreateVolumeInput{
		Size: aws.Int64(8), AvailabilityZone: aws.String(providerTestAZ),
	}, "acct-1")
	require.NoError(t, err)

	assert.Empty(t, provider.lastRequest.SourceSnapshotID)
	assert.Empty(t, provider.lastRequest.SourceSnapshotVolumeID)
}

// fixedAMILoader resolves one AMI to a snapshot on a known source volume.
type fixedAMILoader struct {
	meta           ebsmetadata.AMI
	sourceVolumeID string
}

func (l *fixedAMILoader) GetAMIConfig(_ context.Context, _ string) (ebsmetadata.AMI, error) {
	return l.meta, nil
}

func (l *fixedAMILoader) GetAMISourceVolumeID(_ context.Context, _ string) (string, error) {
	return l.sourceVolumeID, nil
}

// TestDescribeVolumes_Provider_SeesInstanceRootVolume is the end-to-end guard
// for the launch path: a root volume allocated through the provider carries no
// legacy config.json, so it is visible only if the instance service records an
// ebsmetadata document for it.
func TestDescribeVolumes_Provider_SeesInstanceRootVolume(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	cfg := &config.Config{
		AZ:         providerTestAZ,
		WalDir:     t.TempDir(),
		Predastore: config.PredastoreConfig{Bucket: "test-bucket", Host: "127.0.0.1:1", Region: "us-east-1"},
	}
	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})

	// The AMI's snapshot must already exist in the provider, as it would after
	// the image was registered.
	const amiBytes = 4 * 1024 * 1024 * 1024
	_, err := provider.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-origin",
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: amiBytes},
	})
	require.NoError(t, err)
	_, err = provider.CreateSnapshot(context.Background(), ebsprovider.CreateSnapshotRequest{
		Versioned: ebsprovider.NewVersioned(), SnapshotID: "snap-src", VolumeID: "vol-origin",
	})
	require.NoError(t, err)

	instanceSvc := handlers_ec2_instance.NewInstanceServiceImpl(cfg, nil, nil, store, nil, nil, nil)
	instanceSvc.SetEBSProvider(provider)
	instanceSvc.SetRunInstancesDeps(&fixedAMILoader{
		meta:           ebsmetadata.AMI{ImageID: "ami-1", SnapshotID: "snap-src", VolumeSizeGiB: 4},
		sourceVolumeID: "vol-origin",
	}, nil, nil, nil)

	instance := &vm.VM{AccountID: "acct-1"}
	volumeInfos, err := instanceSvc.GenerateVolumes(context.Background(), &ec2.RunInstancesInput{
		ImageId: aws.String("ami-1"),
	}, instance)
	require.NoError(t, err, "the launch must allocate its root volume through the provider")
	require.Len(t, volumeInfos, 1)
	rootVolumeID := volumeInfos[0].VolumeId

	volumeSvc := NewVolumeServiceImplWithStore(cfg, store, nil)
	volumeSvc.SetEBSProvider(provider)

	out, err := volumeSvc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{}, "acct-1")
	require.NoError(t, err)
	require.Len(t, out.Volumes, 1, "the instance root volume must be visible to DescribeVolumes")

	got := out.Volumes[0]
	assert.Equal(t, rootVolumeID, aws.StringValue(got.VolumeId))
	assert.Equal(t, "available", aws.StringValue(got.State))
	assert.Equal(t, providerTestAZ, aws.StringValue(got.AvailabilityZone))
	assert.Equal(t, spxtypes.VolumeTypeGP3, aws.StringValue(got.VolumeType))
	assert.Equal(t, int64(spxtypes.DefaultGP3IOPS), aws.Int64Value(got.Iops))
	assert.Equal(t, "snap-src", aws.StringValue(got.SnapshotId))
}
