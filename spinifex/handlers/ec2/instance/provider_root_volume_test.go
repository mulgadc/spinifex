package handlers_ec2_instance

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	spxtypes "github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

const (
	testRootVolumeBytes = 4 * 1024 * 1024 * 1024
	testRootBucket      = "test-bucket"
	testRootAZ          = "ap-southeast-2a"
	testRootAccount     = "000000000001"
)

// providerRootVolumeService builds an instance service whose only storage
// dependency is the provider. Predastore points at a closed port so any
// fallback to the embedded engine fails loudly instead of silently passing.
func providerRootVolumeService(t *testing.T, provider ebsprovider.EBSProvider, loader AMIMetaLoader, store objectstore.ObjectStore) *InstanceServiceImpl {
	t.Helper()
	return &InstanceServiceImpl{
		config: &config.Config{
			WalDir: t.TempDir(),
			AZ:     testRootAZ,
			Predastore: config.PredastoreConfig{
				Host:   "127.0.0.1:1",
				Bucket: testRootBucket,
				Region: "us-east-1",
			},
		},
		objectStore: store,
		metadata:    ebsmetadata.NewStore(store, testRootBucket),
		amiLoader:   loader,
		ebsProvider: provider,
	}
}

// seedProviderSnapshot registers a source volume and a snapshot of it, which is
// what an AMI's snapshot looks like to the provider.
func seedProviderSnapshot(t *testing.T, provider ebsprovider.EBSProvider, sourceVolumeID, snapshotID string) {
	t.Helper()
	ctx := context.Background()
	_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
		Versioned:     ebsprovider.NewVersioned(),
		VolumeID:      sourceVolumeID,
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: testRootVolumeBytes},
	})
	require.NoError(t, err)
	_, err = provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{
		Versioned: ebsprovider.NewVersioned(), SnapshotID: snapshotID, VolumeID: sourceVolumeID,
	})
	require.NoError(t, err)
}

// rootVolumeAMILoader returns a loader for one AMI backed by snap-source on
// vol-origin, the shape every provider root-volume test needs.
func rootVolumeAMILoader() *fakeAMILoader {
	return &fakeAMILoader{
		byID:       map[string]ebsmetadata.AMI{"ami-1": {ImageID: "ami-1", SnapshotID: "snap-source"}},
		sourceByID: map[string]string{"ami-1": "vol-origin"},
	}
}

// TestPrepareRootVolume_Provider_CreatesFromAMISnapshot locks that an injected
// provider owns root-volume creation: the volume is restored from the AMI's
// snapshot rather than copied block-by-block by the control plane.
func TestPrepareRootVolume_Provider_CreatesFromAMISnapshot(t *testing.T) {
	defer goleak.VerifyNone(t)

	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{CrashConsistentSnapshot: true})
	seedProviderSnapshot(t, provider, "vol-origin", "snap-source")
	svc := providerRootVolumeService(t, provider, rootVolumeAMILoader(), objectstore.NewMemoryObjectStore())

	instance := &vm.VM{AccountID: testRootAccount}
	input := &ec2.RunInstancesInput{ImageId: aws.String("ami-1")}
	err := svc.prepareRootVolume(context.Background(), input, "vol-root", testRootVolumeBytes, 0,
		instance, false)
	require.NoError(t, err)

	created, err := provider.GetVolume(context.Background(), ebsprovider.GetVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-root",
	})
	require.NoError(t, err, "the root volume must exist in the provider")
	assert.Equal(t, int64(testRootVolumeBytes), created.CapacityBytes)

	require.Len(t, instance.EBSRequests.Requests, 1)
	assert.Equal(t, "vol-root", instance.EBSRequests.Requests[0].Name)
	assert.True(t, instance.EBSRequests.Requests[0].Boot, "the root volume must be marked bootable")
	assert.False(t, instance.EBSRequests.Requests[0].DeleteOnTermination)
}

// TestPrepareRootVolume_Provider_WritesMetadataDocument locks the control-plane
// record of the root volume. Without it the volume has neither a document nor a
// legacy config.json, so DescribeVolumes cannot see it at all.
func TestPrepareRootVolume_Provider_WritesMetadataDocument(t *testing.T) {
	defer goleak.VerifyNone(t)

	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})
	seedProviderSnapshot(t, provider, "vol-origin", "snap-source")
	store := objectstore.NewMemoryObjectStore()
	svc := providerRootVolumeService(t, provider, rootVolumeAMILoader(), store)

	instance := &vm.VM{AccountID: testRootAccount}
	err := svc.prepareRootVolume(context.Background(), &ec2.RunInstancesInput{ImageId: aws.String("ami-1")},
		"vol-root", testRootVolumeBytes, 0, instance, true)
	require.NoError(t, err)

	doc, err := ebsmetadata.NewStore(store, testRootBucket).GetVolume(context.Background(), "vol-root")
	require.NoError(t, err, "the root volume must have an ebsmetadata document")

	assert.Equal(t, "vol-root", doc.VolumeID)
	assert.Equal(t, testRootAccount, doc.TenantID)
	assert.Equal(t, uint64(4), doc.CapacityGiB)
	assert.Equal(t, string(ebsprovider.VolumeStateAvailable), doc.State,
		"attach flips the volume to in-use later; creation must not pre-empt it")
	assert.False(t, doc.CreatedAt.IsZero())
	assert.Equal(t, testRootAZ, doc.AvailabilityZone)
	assert.Equal(t, spxtypes.VolumeTypeGP3, doc.VolumeType)
	assert.Equal(t, spxtypes.DefaultGP3IOPS, doc.IOPS)
	assert.Equal(t, spxtypes.DefaultGP3Throughput, doc.Throughput)
	assert.Equal(t, "snap-source", doc.SnapshotID)
	assert.True(t, doc.DeleteOnTermination)
	assert.False(t, doc.Encrypted, "no encryption key is configured in this service")
	assert.NotEmpty(t, doc.ProviderHandle, "the opaque provider handle must be recorded")
	assert.Empty(t, doc.AttachedInstance, "creation must not claim an attachment")
}

// TestPrepareRootVolume_Provider_HonoursRequestedIOPS locks that a launch
// naming Iops in its block device mapping keeps that value.
func TestPrepareRootVolume_Provider_HonoursRequestedIOPS(t *testing.T) {
	defer goleak.VerifyNone(t)

	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})
	seedProviderSnapshot(t, provider, "vol-origin", "snap-source")
	store := objectstore.NewMemoryObjectStore()
	svc := providerRootVolumeService(t, provider, rootVolumeAMILoader(), store)

	err := svc.prepareRootVolume(context.Background(), &ec2.RunInstancesInput{ImageId: aws.String("ami-1")},
		"vol-root", testRootVolumeBytes, 5000, &vm.VM{AccountID: testRootAccount}, true)
	require.NoError(t, err)

	doc, err := ebsmetadata.NewStore(store, testRootBucket).GetVolume(context.Background(), "vol-root")
	require.NoError(t, err)
	assert.Equal(t, 5000, doc.IOPS)
}

// failingPutObjectStore fails writes under failPrefix, so a test can break the
// metadata write without disturbing any other object the launch touches.
type failingPutObjectStore struct {
	objectstore.ObjectStore

	failPrefix string
}

func (s *failingPutObjectStore) PutObject(ctx context.Context, input *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
	if strings.HasPrefix(aws.StringValue(input.Key), s.failPrefix) {
		return nil, errors.New("simulated metadata write failure")
	}
	return s.ObjectStore.PutObject(ctx, input)
}

// TestPrepareRootVolume_Provider_RollbackOnMetadataWriteFailure locks that a
// failed document write deletes the volume the provider just allocated, so a
// failed launch cannot strand an untracked volume.
func TestPrepareRootVolume_Provider_RollbackOnMetadataWriteFailure(t *testing.T) {
	defer goleak.VerifyNone(t)

	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})
	seedProviderSnapshot(t, provider, "vol-origin", "snap-source")
	store := &failingPutObjectStore{
		ObjectStore: objectstore.NewMemoryObjectStore(),
		failPrefix:  "spinifex/ebsmetadata/v1/volumes/",
	}
	svc := providerRootVolumeService(t, provider, rootVolumeAMILoader(), store)

	instance := &vm.VM{AccountID: testRootAccount}
	err := svc.prepareRootVolume(context.Background(), &ec2.RunInstancesInput{ImageId: aws.String("ami-1")},
		"vol-root", testRootVolumeBytes, 0, instance, true)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
	assert.Empty(t, instance.EBSRequests.Requests, "a failed launch must not claim a boot volume")

	_, getErr := provider.GetVolume(context.Background(), ebsprovider.GetVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-root",
	})
	require.ErrorIs(t, getErr, ebsprovider.ErrNotFound, "rollback must delete the just-created provider volume")
}

// TestPrepareRootVolume_Provider_RepeatIsIdempotent locks that re-running the
// same launch does not re-clone or error: the provider returns the existing
// volume when the requested capacity matches.
func TestPrepareRootVolume_Provider_RepeatIsIdempotent(t *testing.T) {
	defer goleak.VerifyNone(t)

	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})
	seedProviderSnapshot(t, provider, "vol-origin", "snap-source")
	svc := providerRootVolumeService(t, provider, rootVolumeAMILoader(), objectstore.NewMemoryObjectStore())
	input := &ec2.RunInstancesInput{ImageId: aws.String("ami-1")}

	for range 2 {
		instance := &vm.VM{AccountID: testRootAccount}
		err := svc.prepareRootVolume(context.Background(), input, "vol-root", testRootVolumeBytes, 0,
			instance, true)
		require.NoError(t, err)
		require.Len(t, instance.EBSRequests.Requests, 1)
	}
}

// TestPrepareRootVolume_Provider_AMIWithoutSnapshotFails locks that an AMI with
// no snapshot still fails, and allocates nothing.
func TestPrepareRootVolume_Provider_AMIWithoutSnapshotFails(t *testing.T) {
	defer goleak.VerifyNone(t)

	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})
	svc := providerRootVolumeService(t, provider, &fakeAMILoader{
		byID: map[string]ebsmetadata.AMI{"ami-1": {ImageID: "ami-1"}},
	}, objectstore.NewMemoryObjectStore())

	instance := &vm.VM{AccountID: testRootAccount}
	err := svc.prepareRootVolume(context.Background(), &ec2.RunInstancesInput{ImageId: aws.String("ami-1")},
		"vol-root", testRootVolumeBytes, 0, instance, true)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
	assert.Empty(t, instance.EBSRequests.Requests, "a failed launch must not claim a boot volume")

	_, getErr := provider.GetVolume(context.Background(), ebsprovider.GetVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-root",
	})
	require.ErrorIs(t, getErr, ebsprovider.ErrNotFound)
}

// TestPrepareRootVolume_Provider_UnknownAMIFails locks the AMI lookup failure
// mapping the legacy clone path already used.
func TestPrepareRootVolume_Provider_UnknownAMIFails(t *testing.T) {
	defer goleak.VerifyNone(t)

	svc := providerRootVolumeService(t, ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}),
		&fakeAMILoader{err: errors.New("missing")}, objectstore.NewMemoryObjectStore())

	instance := &vm.VM{AccountID: testRootAccount}
	err := svc.prepareRootVolume(context.Background(), &ec2.RunInstancesInput{ImageId: aws.String("ami-gone")},
		"vol-root", testRootVolumeBytes, 0, instance, true)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())
	assert.Empty(t, instance.EBSRequests.Requests)
}

// TestPrepareRootVolume_Provider_SourceVolumeUnresolvableFails locks that an
// unresolvable snapshot source aborts rather than sending an incomplete
// CreateVolume the provider would reject.
func TestPrepareRootVolume_Provider_SourceVolumeUnresolvableFails(t *testing.T) {
	defer goleak.VerifyNone(t)

	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})
	seedProviderSnapshot(t, provider, "vol-origin", "snap-source")
	svc := providerRootVolumeService(t, provider, &fakeAMILoader{
		byID:      map[string]ebsmetadata.AMI{"ami-1": {ImageID: "ami-1", SnapshotID: "snap-source"}},
		sourceErr: errors.New("snapshot metadata missing"),
	}, objectstore.NewMemoryObjectStore())

	instance := &vm.VM{AccountID: testRootAccount}
	err := svc.prepareRootVolume(context.Background(), &ec2.RunInstancesInput{ImageId: aws.String("ami-1")},
		"vol-root", testRootVolumeBytes, 0, instance, true)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
	assert.Empty(t, instance.EBSRequests.Requests)

	_, getErr := provider.GetVolume(context.Background(), ebsprovider.GetVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-root",
	})
	require.ErrorIs(t, getErr, ebsprovider.ErrNotFound)
}

// TestPrepareRootVolume_Provider_CreateFailureIsFatal locks that a provider
// error aborts the launch rather than falling back to the embedded engine.
func TestPrepareRootVolume_Provider_CreateFailureIsFatal(t *testing.T) {
	defer goleak.VerifyNone(t)

	// No snapshot seeded, so the provider rejects the clone request.
	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})
	svc := providerRootVolumeService(t, provider, &fakeAMILoader{
		byID:       map[string]ebsmetadata.AMI{"ami-1": {ImageID: "ami-1", SnapshotID: "snap-missing"}},
		sourceByID: map[string]string{"ami-1": "vol-origin"},
	}, objectstore.NewMemoryObjectStore())

	instance := &vm.VM{AccountID: testRootAccount}
	err := svc.prepareRootVolume(context.Background(), &ec2.RunInstancesInput{ImageId: aws.String("ami-1")},
		"vol-root", testRootVolumeBytes, 0, instance, true)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
	assert.Empty(t, instance.EBSRequests.Requests)
}
