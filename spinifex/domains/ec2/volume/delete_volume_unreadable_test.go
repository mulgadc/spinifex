package volume

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	ebsprovider "github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// getFailingStore fails the read of one key with an error that is not a
// missing key, standing in for a document that is present but unreadable
// (e.g. a storage-layer reconstruction failure).
type getFailingStore struct {
	*objectstore.MemoryObjectStore

	failKey string
}

func (s *getFailingStore) GetObject(ctx context.Context, input *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	if aws.StringValue(input.Key) == s.failKey {
		return nil, errors.New("reconstruction failed: shard sizes do not match")
	}
	return s.MemoryObjectStore.GetObject(ctx, input)
}

// countingFailAfterStore fails every read of one key from its (succeedCalls+1)th
// call onward, deterministically reproducing a document that answers one read
// and then goes unreadable on the next, without timing.
type countingFailAfterStore struct {
	*objectstore.MemoryObjectStore

	failKey      string
	succeedCalls int
	calls        int
}

func (s *countingFailAfterStore) GetObject(ctx context.Context, input *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	if aws.StringValue(input.Key) == s.failKey {
		s.calls++
		if s.calls > s.succeedCalls {
			return nil, errors.New("reconstruction failed: shard sizes do not match")
		}
	}
	return s.MemoryObjectStore.GetObject(ctx, input)
}

// TestDeleteVolume_UnreadableDocumentIsInternalError pins the fix: a volume
// document that fails to read for a reason other than absence must answer
// InternalError, never InvalidVolume.NotFound. Reporting NotFound here is
// what let terminate cleanup and the legacy reaper count a volume whose
// state is unknown as already deleted.
func TestDeleteVolume_UnreadableDocumentIsInternalError(t *testing.T) {
	memory := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", memory)

	volumeID := "vol-unreadable0000"
	createVolumeInStoreWithMeta(t, svc, memory, volumeID, ebsmetadata.Volume{
		VolumeID: volumeID, CapacityGiB: 10, State: "available",
	})

	failing := &getFailingStore{MemoryObjectStore: memory, failKey: metadataKey(t, volumeID)}
	svc.store = failing
	svc.metadata = ebsmetadata.NewStore(failing, "test-bucket")

	_, err := svc.DeleteVolume(context.Background(), &ec2.DeleteVolumeInput{
		VolumeId: aws.String(volumeID),
	}, testVolAccountID)

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, awserrors.ValidErrorCodeFromError(err),
		"an unreadable document must answer InternalError, not NotFound")
	assert.NotContains(t, err.Error(), awserrors.ErrorInvalidVolumeNotFound)

	// Nothing was deleted: the document is still there (read through the
	// underlying store, bypassing the fault), and so is the provider volume.
	_, getErr := ebsmetadata.NewStore(memory, "test-bucket").GetVolume(context.Background(), testVolAccountID, volumeID)
	require.NoError(t, getErr, "the control-plane document must survive a failed delete")

	provVol, provErr := svc.provider.GetVolume(context.Background(), ebsprovider.GetVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID,
	})
	require.NoError(t, provErr)
	require.NotNil(t, provVol, "the provider's data must survive a failed delete")
}

// TestDeleteVolume_AbsentDocumentIsStillNotFound guards the other side of the
// fix: absence (NoSuchKey) must keep returning InvalidVolume.NotFound, not be
// swept up into the new InternalError branch.
func TestDeleteVolume_AbsentDocumentIsStillNotFound(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")

	_, err := svc.DeleteVolume(context.Background(), &ec2.DeleteVolumeInput{
		VolumeId: aws.String("vol-absent00000001"),
	}, testVolAccountID)

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, err.Error())
}

// TestDeleteVolume_CrossAccountDocumentIsStillNotFound guards the cross-tenant
// branch: a document that exists but belongs to another account must keep
// returning InvalidVolume.NotFound, exactly as before the fix.
func TestDeleteVolume_CrossAccountDocumentIsStillNotFound(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-othertenant000", ebsmetadata.Volume{
		VolumeID: "vol-othertenant000", CapacityGiB: 10, State: "available", TenantID: "222222222222",
	})

	_, err := svc.DeleteVolume(context.Background(), &ec2.DeleteVolumeInput{
		VolumeId: aws.String("vol-othertenant000"),
	}, testVolAccountID)

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, err.Error())
}

// TestDeleteVolumeOnTerminate_DeleteReadFailureIsNotSuccess reproduces the
// production shape from mulga-salhr: the attachment-clear read inside
// UpdateVolumeState succeeds, but the document goes unreadable before
// DeleteVolume's own read of the same key. That read failure must come back
// as a non-NotFound error, which is exactly the signal
// runtime/daemon/vm_adapters.go's DeleteVolumes and instance/service_impl.go's
// deleteInstanceVolumes use to tell a real failure from an already-gone
// volume (!awserrors.IsNotFound(err) && !objectstore.IsNoSuchKeyError(err)).
func TestDeleteVolumeOnTerminate_DeleteReadFailureIsNotSuccess(t *testing.T) {
	memory := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", memory)

	volumeID := "vol-stopped-root2"
	createVolumeInStoreWithMeta(t, svc, memory, volumeID, ebsmetadata.Volume{
		VolumeID:         volumeID,
		CapacityGiB:      10,
		State:            "available",
		AttachedInstance: "i-stopped",
	})

	// The first read of the document (UpdateVolumeState's attachment clear)
	// succeeds; every read from the second onward (DeleteVolume's own read)
	// fails, deterministically, with no reliance on timing.
	failing := &countingFailAfterStore{MemoryObjectStore: memory, failKey: metadataKey(t, volumeID), succeedCalls: 1}
	svc.store = failing
	svc.metadata = ebsmetadata.NewStore(failing, "test-bucket")

	err := svc.DeleteVolumeOnTerminate(context.Background(), volumeID, testVolAccountID)

	require.Error(t, err, "a delete-read failure after a successful attachment clear must not be reported as success")
	assert.False(t, awserrors.IsNotFound(err),
		"terminate cleanup treats IsNotFound as already-gone; this must not qualify")
	assert.False(t, objectstore.IsNoSuchKeyError(err),
		"terminate cleanup treats IsNoSuchKeyError as already-gone; this must not qualify")

	// The attachment was cleared (terminate implies detach), but the document
	// and the provider volume must both still exist: nothing was deleted.
	_, getErr := ebsmetadata.NewStore(memory, "test-bucket").GetVolume(context.Background(), testVolAccountID, volumeID)
	require.NoError(t, getErr, "the control-plane document must survive a failed delete")

	provVol, provErr := svc.provider.GetVolume(context.Background(), ebsprovider.GetVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID,
	})
	require.NoError(t, provErr)
	require.NotNil(t, provVol, "the provider's data must survive a failed delete")
}

// TestForceDetachVolume_UnreadableDocumentIsInternalError: an attached volume
// whose document is present but unreadable must answer InternalError and keep
// its attachment; absence and another account's volume stay NotFound.
func TestForceDetachVolume_UnreadableDocumentIsInternalError(t *testing.T) {
	memory := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", memory)

	volumeID := "vol-forcedetach000"
	createVolumeInStoreWithMeta(t, svc, memory, volumeID, ebsmetadata.Volume{
		VolumeID: volumeID, CapacityGiB: 10, State: "in-use", AttachedInstance: "i-0123456789abcdef0",
	})

	failing := &getFailingStore{MemoryObjectStore: memory, failKey: metadataKey(t, volumeID)}
	svc.store = failing
	svc.metadata = ebsmetadata.NewStore(failing, "test-bucket")

	_, err := svc.ForceDetachVolume(context.Background(), &ec2.DetachVolumeInput{
		VolumeId: aws.String(volumeID),
	}, testVolAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, awserrors.ValidErrorCodeFromError(err))

	meta, getErr := ebsmetadata.NewStore(memory, "test-bucket").GetVolume(context.Background(), testVolAccountID, volumeID)
	require.NoError(t, getErr)
	assert.Equal(t, "in-use", meta.State, "a failed force-detach must not clear the attachment")
	assert.Equal(t, "i-0123456789abcdef0", meta.AttachedInstance)

	_, err = svc.ForceDetachVolume(context.Background(), &ec2.DetachVolumeInput{
		VolumeId: aws.String("vol-doesnotexist00"),
	}, testVolAccountID)
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, awserrors.ValidErrorCodeFromError(err))

	_, err = svc.ForceDetachVolume(context.Background(), &ec2.DetachVolumeInput{
		VolumeId: aws.String(volumeID),
	}, "999999999999")
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, awserrors.ValidErrorCodeFromError(err))
}
