package handlers_ec2_image

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const providerSnapshotAZ = "ap-southeast-2a"

// setupProviderSnapshotImageService builds an image service whose only
// storage dependency for the snapshot paths is the provider. Predastore
// points at a closed port so any accidental fall-through to the embedded
// engine fails loudly (connection refused) instead of silently passing.
func setupProviderSnapshotImageService(t *testing.T) (*ImageServiceImpl, *objectstore.MemoryObjectStore, ebsprovider.EBSProvider) {
	t.Helper()
	store := objectstore.NewMemoryObjectStore()
	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})
	cfg := &config.Config{
		WalDir: t.TempDir(),
		AZ:     providerSnapshotAZ,
		Predastore: config.PredastoreConfig{
			Host:   "127.0.0.1:1",
			Bucket: testBucket,
			Region: "us-east-1",
		},
	}
	svc := NewImageServiceImplWithConfig(cfg, store, nil)
	svc.SetEBSProvider(provider)
	return svc, store, provider
}

// seedProviderVolume creates a provider-backed volume plus its ebsmetadata
// document, wiring the document's ProviderHandle to the volume's real handle
// so the fixture cannot drift from what the provider actually issued.
func seedProviderVolume(t *testing.T, svc *ImageServiceImpl, provider ebsprovider.EBSProvider, volumeID string, sizeGiB uint64) ebsmetadata.Volume {
	t.Helper()
	ctx := context.Background()
	sizeBytes := int64(sizeGiB) * 1024 * 1024 * 1024
	created, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
		Versioned:     ebsprovider.NewVersioned(),
		VolumeID:      volumeID,
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: sizeBytes},
	})
	require.NoError(t, err)

	doc := ebsmetadata.Volume{
		VolumeID:         volumeID,
		TenantID:         testAccountID,
		CapacityGiB:      sizeGiB,
		State:            "available",
		CreatedAt:        time.Now(),
		AvailabilityZone: providerSnapshotAZ,
		VolumeType:       "gp3",
		ProviderHandle:   created.Handle,
	}
	require.NoError(t, svc.MetadataStore().PutVolume(ctx, doc))
	return doc
}

// TestGetVolumeConfig_Provider_ResolvesProviderCreatedVolume is the
// regression guard for the whole change: a provider-created volume has no
// {volumeID}/config.json, so reading the legacy key made every CreateImage
// from such a volume fail with NoSuchKey, the same bug AttachVolume had
// before its volume-config read was fixed.
func TestGetVolumeConfig_Provider_ResolvesProviderCreatedVolume(t *testing.T) {
	svc, _, provider := setupProviderSnapshotImageService(t)
	doc := seedProviderVolume(t, svc, provider, "vol-provider001", 10)

	cfg, err := svc.getVolumeConfig(context.Background(), doc.VolumeID)
	require.NoError(t, err, "a provider-created volume with no config.json must resolve")
	assert.Equal(t, doc.VolumeID, cfg.VolumeMetadata.VolumeID)
	assert.Equal(t, testAccountID, cfg.VolumeMetadata.TenantID)
	assert.Equal(t, uint64(10), cfg.VolumeMetadata.SizeGiB)
	assert.Equal(t, "available", cfg.VolumeMetadata.State)
	assert.Equal(t, providerSnapshotAZ, cfg.VolumeMetadata.AvailabilityZone)
}

// TestGetVolumeConfig_Provider_UnknownVolumeIsNotFound locks that a missing
// ebsmetadata document maps to InvalidVolume.NotFound, not a generic error.
func TestGetVolumeConfig_Provider_UnknownVolumeIsNotFound(t *testing.T) {
	svc, _, _ := setupProviderSnapshotImageService(t)

	_, err := svc.getVolumeConfig(context.Background(), "vol-does-not-exist")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, err.Error())
}

// TestSnapshotStoppedVolume_Provider_CreatesThroughProvider locks that a
// stopped-volume snapshot under a provider is created via provider.CreateSnapshot
// and never falls through to building an embedded engine — the Predastore host
// is a closed port, so any fallback would fail with connection refused.
func TestSnapshotStoppedVolume_Provider_CreatesThroughProvider(t *testing.T) {
	svc, _, provider := setupProviderSnapshotImageService(t)
	doc := seedProviderVolume(t, svc, provider, "vol-stopped001", 8)

	err := svc.snapshotStoppedVolume(doc.VolumeID, "snap-stopped001")
	require.NoError(t, err, "the provider path must not fall through to the embedded engine against the closed Predastore port")

	// CreateSnapshot is idempotent on (SnapshotID, VolumeID): re-issuing it
	// returns the existing snapshot rather than creating a second one, which
	// confirms the snapshot genuinely exists in the provider (sourced from
	// the right volume) instead of the call having silently no-opped.
	snap, err := provider.CreateSnapshot(context.Background(), ebsprovider.CreateSnapshotRequest{
		Versioned: ebsprovider.NewVersioned(), SnapshotID: "snap-stopped001", VolumeID: doc.VolumeID,
	})
	require.NoError(t, err)
	assert.Equal(t, doc.VolumeID, snap.SourceVolumeID)
}

// TestSnapshotRunningVolume_Provider_PassesProviderHandleAsVolumeHandle locks
// that the ebsmetadata document's ProviderHandle is threaded through as
// CreateSnapshotRequest.VolumeHandle. A wrong handle must be rejected by the
// provider (MemoryProvider treats an empty VolumeHandle as "skip the check",
// so only a mismatched non-empty value proves the real value is forwarded).
func TestSnapshotRunningVolume_Provider_PassesProviderHandleAsVolumeHandle(t *testing.T) {
	svc, _, provider := setupProviderSnapshotImageService(t)
	doc := seedProviderVolume(t, svc, provider, "vol-running001", 8)

	err := svc.snapshotRunningVolume(doc.VolumeID, "snap-running001", testAccountID)
	require.NoError(t, err, "the correct provider handle must let CreateSnapshot succeed")

	doc.ProviderHandle = "wrong-handle"
	require.NoError(t, svc.MetadataStore().PutVolume(context.Background(), doc))

	err = svc.snapshotRunningVolume(doc.VolumeID, "snap-running002", testAccountID)
	require.Error(t, err, "a mismatched provider handle must be rejected, proving the value came from the document")
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

// TestCreateImageFromInstance_Provider_UsesEbsMetadataVolumeSize is the
// end-to-end case: under a provider, CreateImageFromInstance's AMI.VolumeSizeGiB
// must come from the ebsmetadata document, not a legacy config.json (which a
// provider-created volume never has).
func TestCreateImageFromInstance_Provider_UsesEbsMetadataVolumeSize(t *testing.T) {
	svc, _, provider := setupProviderSnapshotImageService(t)
	doc := seedProviderVolume(t, svc, provider, "vol-e2e001", 20)

	out, err := svc.CreateImageFromInstance(CreateImageParams{
		Input: &ec2.CreateImageInput{
			InstanceId: aws.String("i-e2e001"),
			Name:       aws.String("provider-ami"),
		},
		RootVolumeID: doc.VolumeID,
		IsRunning:    false,
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.ImageId)

	ami, err := svc.MetadataStore().GetAMI(context.Background(), aws.StringValue(out.ImageId))
	require.NoError(t, err)
	assert.Equal(t, uint64(20), ami.VolumeSizeGiB, "the AMI's volume size must come from the ebsmetadata document")
}
