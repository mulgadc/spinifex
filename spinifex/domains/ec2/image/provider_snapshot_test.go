package image

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/nats-io/nats.go"
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

// TestGetVolumeMetadata_Provider_ResolvesProviderCreatedVolume is the
// regression guard for the whole change: a provider-created volume has no
// {volumeID}/config.json, so reading the legacy key made every CreateImage
// from such a volume fail with NoSuchKey, the same bug AttachVolume had
// before its volume-config read was fixed.
func TestGetVolumeMetadata_Provider_ResolvesProviderCreatedVolume(t *testing.T) {
	svc, _, provider := setupProviderSnapshotImageService(t)
	doc := seedProviderVolume(t, svc, provider, "vol-provider001", 10)

	meta, err := svc.getVolumeMetadata(context.Background(), testAccountID, doc.VolumeID)
	require.NoError(t, err, "a provider-created volume with no config.json must resolve")
	assert.Equal(t, doc.VolumeID, meta.VolumeID)
	assert.Equal(t, testAccountID, meta.TenantID)
	assert.Equal(t, uint64(10), meta.CapacityGiB)
	assert.Equal(t, "available", meta.State)
	assert.Equal(t, providerSnapshotAZ, meta.AvailabilityZone)
}

// TestGetVolumeMetadata_Provider_UnknownVolumeIsNotFound locks that a missing
// ebsmetadata document maps to InvalidVolume.NotFound, not a generic error.
func TestGetVolumeMetadata_Provider_UnknownVolumeIsNotFound(t *testing.T) {
	svc, _, _ := setupProviderSnapshotImageService(t)

	_, err := svc.getVolumeMetadata(context.Background(), testAccountID, "vol-does-not-exist")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, err.Error())
}

// TestSnapshotVolume_Provider_CreatesThroughProvider locks that a
// root-volume snapshot under a provider is created via provider.CreateSnapshot
// and never falls through to building an embedded engine — the Predastore host
// is a closed port, so any fallback would fail with connection refused.
func TestSnapshotVolume_Provider_CreatesThroughProvider(t *testing.T) {
	svc, _, provider := setupProviderSnapshotImageService(t)
	doc := seedProviderVolume(t, svc, provider, "vol-stopped001", 8)

	err := svc.snapshotVolume(doc.VolumeID, "snap-stopped001", testAccountID)
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

// TestSnapshotVolume_Provider_PassesProviderHandleAsVolumeHandle locks
// that the ebsmetadata document's ProviderHandle is threaded through as
// CreateSnapshotRequest.VolumeHandle. A wrong handle must be rejected by the
// provider (MemoryProvider treats an empty VolumeHandle as "skip the check",
// so only a mismatched non-empty value proves the real value is forwarded).
func TestSnapshotVolume_Provider_PassesProviderHandleAsVolumeHandle(t *testing.T) {
	svc, _, provider := setupProviderSnapshotImageService(t)
	doc := seedProviderVolume(t, svc, provider, "vol-running001", 8)

	err := svc.snapshotVolume(doc.VolumeID, "snap-running001", testAccountID)
	require.NoError(t, err, "the correct provider handle must let CreateSnapshot succeed")

	doc.ProviderHandle = "wrong-handle"
	require.NoError(t, svc.MetadataStore().PutVolume(context.Background(), doc))

	err = svc.snapshotVolume(doc.VolumeID, "snap-running002", testAccountID)
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

// An image of a running disk is no longer the imported artifact, so it must
// not inherit the source AMI's digest even though it inherits other fields.
func TestCreateImageFromInstance_DoesNotInheritSourceDigest(t *testing.T) {
	svc, _, provider := setupProviderSnapshotImageService(t)
	doc := seedProviderVolume(t, svc, provider, "vol-digest001", 8)
	require.NoError(t, svc.MetadataStore().PutAMI(context.Background(), ebsmetadata.AMI{
		ImageID:         "ami-src-digest",
		Name:            "imported",
		ImageOwnerAlias: "system",
		BootMode:        "uefi",
		SourceDigest: &ebsmetadata.ImageDigest{
			Algorithm: "sha256", Value: "abc123", Verification: ebsmetadata.DigestOperator, Filename: "img.raw",
		},
	}))

	out, err := svc.CreateImageFromInstance(CreateImageParams{
		Input: &ec2.CreateImageInput{
			InstanceId: aws.String("i-digest001"),
			Name:       aws.String("from-instance"),
		},
		RootVolumeID:  doc.VolumeID,
		SourceImageID: "ami-src-digest",
		IsRunning:     false,
	}, testAccountID)
	require.NoError(t, err)

	ami, err := svc.MetadataStore().GetAMI(context.Background(), aws.StringValue(out.ImageId))
	require.NoError(t, err)
	assert.Equal(t, "uefi", ami.BootMode, "the source AMI must have been read")
	assert.Nil(t, ami.SourceDigest)
}

// AWS takes an image's EnaSupport from the instance it was made from, and every
// instance here reports it, so the source AMI's value is not inherited.
func TestCreateImageFromInstance_SetsEnaSupportFromInstance(t *testing.T) {
	svc, _, provider := setupProviderSnapshotImageService(t)
	doc := seedProviderVolume(t, svc, provider, "vol-ena001", 8)
	require.NoError(t, svc.MetadataStore().PutAMI(context.Background(), ebsmetadata.AMI{
		ImageID:         "ami-src-ena",
		Name:            "no-ena",
		ImageOwnerAlias: testAccountID,
		EnaSupport:      aws.Bool(false),
	}))

	out, err := svc.CreateImageFromInstance(CreateImageParams{
		Input: &ec2.CreateImageInput{
			InstanceId: aws.String("i-ena001"),
			Name:       aws.String("from-no-ena"),
		},
		RootVolumeID:  doc.VolumeID,
		SourceImageID: "ami-src-ena",
		IsRunning:     false,
	}, testAccountID)
	require.NoError(t, err)

	ami, err := svc.MetadataStore().GetAMI(context.Background(), aws.StringValue(out.ImageId))
	require.NoError(t, err)
	require.NotNil(t, ami.EnaSupport)
	assert.True(t, *ami.EnaSupport)
}

// createUnavailableProvider refuses every CreateSnapshot as retry-later.
type createUnavailableProvider struct {
	ebsprovider.EBSProvider
}

func (p *createUnavailableProvider) CreateSnapshot(context.Context, ebsprovider.CreateSnapshotRequest) (*ebsprovider.Snapshot, error) {
	return nil, &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeUnavailable, Message: "held by node-2"}
}

// TestSnapshotVolume_Provider_UnavailableIsRetryLater locks that the
// CreateImage snapshot surfaces a provider's retry-later as ServiceUnavailable.
func TestSnapshotVolume_Provider_UnavailableIsRetryLater(t *testing.T) {
	svc, _, provider := setupProviderSnapshotImageService(t)
	doc := seedProviderVolume(t, svc, provider, "vol-stopped-busy", 8)
	svc.SetEBSProvider(&createUnavailableProvider{EBSProvider: provider})

	err := svc.snapshotVolume(doc.VolumeID, "snap-stopped-busy", testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServiceUnavailable, err.Error())
}

// createInUseProvider refuses every CreateSnapshot as volume in use.
type createInUseProvider struct {
	ebsprovider.EBSProvider
}

func (p *createInUseProvider) CreateSnapshot(context.Context, ebsprovider.CreateSnapshotRequest) (*ebsprovider.Snapshot, error) {
	return nil, &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeVolumeInUse, Message: "published"}
}

// TestSnapshotVolume_Provider_VolumeInUseIsIncorrectState locks that the
// CreateImage snapshot tells the caller to detach a volume the provider refuses as in use.
func TestSnapshotVolume_Provider_VolumeInUseIsIncorrectState(t *testing.T) {
	svc, _, provider := setupProviderSnapshotImageService(t)
	doc := seedProviderVolume(t, svc, provider, "vol-attached-inuse", 8)
	svc.SetEBSProvider(&createInUseProvider{EBSProvider: provider})

	err := svc.snapshotVolume(doc.VolumeID, "snap-attached-inuse", testAccountID)

	code, message, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok, "unregistered error: %v", err)
	assert.Equal(t, awserrors.ErrorIncorrectState, code)
	assert.Contains(t, message, doc.VolumeID)
	assert.Contains(t, message, "Detach")
}

// countingSnapshotProvider counts the CreateSnapshot calls it forwards.
type countingSnapshotProvider struct {
	ebsprovider.EBSProvider

	creates atomic.Int32
}

func (p *countingSnapshotProvider) CreateSnapshot(ctx context.Context, req ebsprovider.CreateSnapshotRequest) (*ebsprovider.Snapshot, error) {
	p.creates.Add(1)
	return p.EBSProvider.CreateSnapshot(ctx, req)
}

// An image of a running instance is snapshotted by the provider, which drains
// a buffered volume itself, so CreateImage neither dials the drain socket nor
// asks the instance's host to drain.
func TestCreateImageFromInstance_RunningInstanceLeavesTheDrainToTheProvider(t *testing.T) {
	const instanceID = "i-running-drain"
	svc, _, provider := setupProviderSnapshotImageService(t)
	_, nc := testutil.StartTestNATS(t)
	svc.natsConn = nc
	svc.config.DataDir = testutil.SocketTempDir(t)
	counting := &countingSnapshotProvider{EBSProvider: provider}
	svc.SetEBSProvider(counting)

	doc := seedProviderVolume(t, svc, provider, "vol-running-drain", 8)
	doc.State, doc.AttachedInstance = "in-use", instanceID
	require.NoError(t, svc.MetadataStore().PutVolume(context.Background(), doc))
	drains := testutil.StartDrainSocket(t, svc.config.DataDir, doc.VolumeID, "OK\n")
	var commands atomic.Int32
	sub, err := nc.Subscribe(ec2v1.InstanceCommandSubject(instanceID), func(msg *nats.Msg) {
		commands.Add(1)
		_ = msg.Respond([]byte(`{"Code":"InternalError","Message":"no drain socket"}`))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	out, err := svc.CreateImageFromInstance(CreateImageParams{
		Input:        &ec2.CreateImageInput{InstanceId: aws.String(instanceID), Name: aws.String("running-ami")},
		RootVolumeID: doc.VolumeID,
		IsRunning:    true,
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.ImageId)

	assert.Equal(t, int32(1), counting.creates.Load(), "the provider must take the snapshot")
	assert.Zero(t, drains(), "CreateImage must not dial the drain socket")
	assert.Zero(t, commands.Load(), "CreateImage must not ask the instance's host to drain")
}
