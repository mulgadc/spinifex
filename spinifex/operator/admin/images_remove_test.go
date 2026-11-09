package admin

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	awss3 "github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testRemoveBucket    = "test-bucket"
	testRemoveAccountID = "000000000001"
)

// putAMI registers an AMI the only way the control plane knows one: as an
// ebsmetadata document.
func putAMI(t *testing.T, store *objectstore.MemoryObjectStore, imageID, name, owner, snapshotID string) {
	t.Helper()
	require.NoError(t, ebsmetadata.NewStore(store, testRemoveBucket).PutAMI(t.Context(), ebsmetadata.AMI{
		ImageID:         imageID,
		Name:            name,
		ImageOwnerAlias: owner,
		SnapshotID:      snapshotID,
		VolumeSizeGiB:   8,
	}))
}

// putCorruptAMI writes an undecodable document at the AMI's key, the state the
// salvage path exists for: present, so not not-found, but unreadable.
func putCorruptAMI(t *testing.T, store *objectstore.MemoryObjectStore, imageID string) {
	t.Helper()
	key, err := ebsmetadata.AMIKey(imageID)
	require.NoError(t, err)
	_, err = store.PutObject(t.Context(), &awss3.PutObjectInput{
		Bucket: aws.String(testRemoveBucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader([]byte("{not valid json")),
	})
	require.NoError(t, err)
}

// removeProvider is a memory provider that records the deletes it is asked
// for, in order, and can fail GetVolume or DeleteSnapshot.
type removeProvider struct {
	*ebsprovider.MemoryProvider
	deletes           []string
	getVolumeErr      error
	deleteSnapshotErr error
}

func newRemoveProvider() *removeProvider {
	return &removeProvider{MemoryProvider: ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{CrashConsistentSnapshot: true})}
}

func (p *removeProvider) GetVolume(ctx context.Context, req ebsprovider.GetVolumeRequest) (*ebsprovider.Volume, error) {
	if p.getVolumeErr != nil {
		return nil, p.getVolumeErr
	}
	return p.MemoryProvider.GetVolume(ctx, req)
}

func (p *removeProvider) DeleteSnapshot(ctx context.Context, req ebsprovider.DeleteSnapshotRequest) error {
	p.deletes = append(p.deletes, "snapshot "+req.SnapshotID)
	if p.deleteSnapshotErr != nil {
		return p.deleteSnapshotErr
	}
	return p.MemoryProvider.DeleteSnapshot(ctx, req)
}

func (p *removeProvider) DeleteVolume(ctx context.Context, req ebsprovider.DeleteVolumeRequest) error {
	p.deletes = append(p.deletes, "volume "+req.VolumeID)
	return p.MemoryProvider.DeleteVolume(ctx, req)
}

// putImageStorage gives the provider what an admin import leaves: a volume
// named after the image and its snapshot at SnapPrefix.
func putImageStorage(t *testing.T, provider *removeProvider, imageID string, capacity int64) {
	t.Helper()
	_, err := provider.CreateVolume(t.Context(), ebsprovider.CreateVolumeRequest{
		Versioned:     ebsprovider.NewVersioned(),
		VolumeID:      imageID,
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: capacity},
	})
	require.NoError(t, err)
	_, err = provider.CreateSnapshot(t.Context(), ebsprovider.CreateSnapshotRequest{
		Versioned:  ebsprovider.NewVersioned(),
		SnapshotID: SnapPrefix(imageID),
		VolumeID:   imageID,
	})
	require.NoError(t, err)
}

// assertImageStorageGone checks the provider holds neither the image's volume
// nor its snapshot.
func assertImageStorageGone(t *testing.T, provider *removeProvider, imageID string) {
	t.Helper()
	_, err := provider.MemoryProvider.GetVolume(t.Context(), ebsprovider.GetVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: imageID})
	require.ErrorIs(t, err, ebsprovider.ErrNotFound, "the image volume must be deleted")
	_, ok := provider.Snapshot(SnapPrefix(imageID))
	assert.False(t, ok, "the image snapshot must be deleted")
}

// wantDeletes is the provider calls a removal makes: snapshot, then volume.
func wantDeletes(imageID string) []string {
	return []string{"snapshot " + SnapPrefix(imageID), "volume " + imageID}
}

// putSnapMetadata writes a CopyImage-derived EC2 snapshot metadata that points
// VolumeID back at an admin-imported AMI's ID.
func putSnapMetadata(t *testing.T, store *objectstore.MemoryObjectStore, snapID, volumeID string) {
	t.Helper()
	require.NoError(t, ebsmetadata.NewStore(store, testRemoveBucket).PutSnapshot(t.Context(), ebsmetadata.Snapshot{
		SnapshotID: snapID,
		VolumeID:   volumeID,
		VolumeSize: 8,
		State:      "completed",
		OwnerID:    testRemoveAccountID,
	}))
}

// putVolume registers a volume document carrying the given SnapshotID.
func putVolume(t *testing.T, store *objectstore.MemoryObjectStore, volID, snapshotID string) {
	t.Helper()
	require.NoError(t, ebsmetadata.NewStore(store, testRemoveBucket).PutVolume(t.Context(), ebsmetadata.Volume{
		VolumeID:    volID,
		TenantID:    testRemoveAccountID,
		SnapshotID:  snapshotID,
		CapacityGiB: 8,
	}))
}

func TestRemoveSystemImage_HappyPath(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	provider := newRemoveProvider()
	const id = "ami-sys-deb13"
	putAMI(t, store, id, "debian-13", "system", SnapPrefix(id))
	putImageStorage(t, provider, id, 8<<30)

	preview, err := PreviewRemoveSystemImage(t.Context(), store, provider, testRemoveBucket, id)
	require.NoError(t, err)
	assert.True(t, preview.ConfigPresent)
	assert.True(t, preview.IsSystemOwned)
	assert.Equal(t, "system", preview.Owner)
	assert.Equal(t, VolumePresent, preview.VolumeStatus)
	assert.Equal(t, int64(8<<30), preview.VolumeCapacityBytes)
	assert.Equal(t, SnapPrefix(id), preview.SnapshotID)
	assert.True(t, preview.Dependents.Empty())
	assert.Empty(t, provider.deletes, "the preview deletes nothing")

	res, err := RemoveSystemImage(t.Context(), store, provider, testRemoveBucket, RemoveImageOpts{ImageID: id})
	require.NoError(t, err)
	assert.Equal(t, id, res.VolumeID)
	assert.Equal(t, SnapPrefix(id), res.SnapshotID)
	assert.Equal(t, wantDeletes(id), provider.deletes, "the snapshot goes before the volume")
	assertImageStorageGone(t, provider, id)
	assert.Equal(t, 0, store.Count())
}

// TestRemoveSystemImage_OrphanedBlocksAreReclaimable covers the state the
// orphan report points operators at: a provider volume with no document.
func TestRemoveSystemImage_OrphanedBlocksAreReclaimable(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	provider := newRemoveProvider()
	const id = "ami-orphaned0001"
	putImageStorage(t, provider, id, 1<<30)

	_, err := RemoveSystemImage(t.Context(), store, provider, testRemoveBucket, RemoveImageOpts{ImageID: id, Force: true})
	require.NoError(t, err, "an image whose document is already gone must still have its storage reclaimable")
	assert.Equal(t, wantDeletes(id), provider.deletes)
	assertImageStorageGone(t, provider, id)
}

// A volume and snapshot another remover already deleted are done: the
// provider's deletes are idempotent on an absent target.
func TestRemoveSystemImage_AbsentProviderTargetsAreDone(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	provider := newRemoveProvider()
	const id = "ami-racing000001"

	res, err := RemoveSystemImage(t.Context(), store, provider, testRemoveBucket, RemoveImageOpts{ImageID: id, Force: true})
	require.NoError(t, err)
	assert.Equal(t, wantDeletes(id), provider.deletes)
	assert.Equal(t, id, res.VolumeID)
}

// A failed snapshot delete stops the removal before the volume, so the
// snapshot never outlives the blocks it may refer to.
func TestRemoveSystemImage_SnapshotDeleteFailureKeepsTheVolume(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	provider := newRemoveProvider()
	provider.deleteSnapshotErr = &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeInternal, Message: "delete snapshot objects: boom"}
	const id = "ami-sys-snapfail"
	putAMI(t, store, id, "debian-13", "system", SnapPrefix(id))
	putImageStorage(t, provider, id, 1<<30)

	_, err := RemoveSystemImage(t.Context(), store, provider, testRemoveBucket, RemoveImageOpts{ImageID: id})
	require.Error(t, err)
	assert.Contains(t, err.Error(), SnapPrefix(id))
	assert.Equal(t, []string{"snapshot " + SnapPrefix(id)}, provider.deletes, "DeleteVolume must not be called")
	_, err = provider.MemoryProvider.GetVolume(t.Context(), ebsprovider.GetVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: id})
	require.NoError(t, err, "the volume stays for a rerun")
}

// viperblockd answers internal when a volume's persisted state is unreadable,
// the half-removed image --force salvages: the preview reports the capacity
// as unknown instead of failing, and the removal still deletes both.
func TestRemoveSystemImage_UnreadableVolumePreviewsUnknownAndForceRemoves(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	provider := newRemoveProvider()
	const id = "ami-unreadable01"
	putImageStorage(t, provider, id, 1<<30)
	providerErr := &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeInternal, Message: "describe volume: state unreadable"}
	provider.getVolumeErr = providerErr

	preview, err := PreviewRemoveSystemImage(t.Context(), store, provider, testRemoveBucket, id)
	require.NoError(t, err, "a GetVolume failure must not abort the preview")
	assert.False(t, preview.ConfigPresent)
	assert.Equal(t, VolumeUnknown, preview.VolumeStatus)
	require.ErrorIs(t, preview.VolumeErr, providerErr)
	assert.Zero(t, preview.VolumeCapacityBytes)

	_, err = RemoveSystemImage(t.Context(), store, provider, testRemoveBucket, RemoveImageOpts{ImageID: id, Force: true})
	require.NoError(t, err)
	assert.Equal(t, wantDeletes(id), provider.deletes)
	assertImageStorageGone(t, provider, id)
}

func TestRemoveSystemImage_AccountOwned_Refused(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	provider := newRemoveProvider()
	const id = "ami-acct-001"
	putAMI(t, store, id, "user-ami", testRemoveAccountID, "snap-acct-001")
	putImageStorage(t, provider, id, 1<<30)

	_, err := RemoveSystemImage(t.Context(), store, provider, testRemoveBucket, RemoveImageOpts{ImageID: id})
	require.Error(t, err)
	assert.Contains(t, err.Error(), id)
	assert.Contains(t, err.Error(), testRemoveAccountID)
	assert.Contains(t, err.Error(), "account-owned")
	assert.Contains(t, err.Error(), "aws ec2 deregister-image")
	assert.Empty(t, provider.deletes, "the provider is not asked to delete anything")
	assert.Equal(t, 1, store.Count(), "the AMI document stays")
}

func TestRemoveSystemImage_AccountOwned_ForceBypasses(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	provider := newRemoveProvider()
	const id = "ami-acct-002"
	putAMI(t, store, id, "user-ami", testRemoveAccountID, "snap-acct-002")
	putImageStorage(t, provider, id, 1<<30)

	_, err := RemoveSystemImage(t.Context(), store, provider, testRemoveBucket, RemoveImageOpts{ImageID: id, Force: true})
	require.NoError(t, err)
	assertImageStorageGone(t, provider, id)
	assert.Equal(t, 0, store.Count())
}

func TestRemoveSystemImage_MissingConfig_NotFound(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	provider := newRemoveProvider()
	const id = "ami-missing"
	putImageStorage(t, provider, id, 1<<30)

	_, err := RemoveSystemImage(t.Context(), store, provider, testRemoveBucket, RemoveImageOpts{ImageID: id})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())
	assert.Empty(t, provider.deletes)
}

func TestRemoveSystemImage_CorruptConfig_NotFound(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	provider := newRemoveProvider()
	const id = "ami-corrupt"
	putCorruptAMI(t, store, id)

	_, err := RemoveSystemImage(t.Context(), store, provider, testRemoveBucket, RemoveImageOpts{ImageID: id})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())
	assert.Empty(t, provider.deletes)
}

func TestRemoveSystemImage_DependentVolume_Direct_Refused(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	provider := newRemoveProvider()
	const id = "ami-sys-deb13"
	putAMI(t, store, id, "debian-13", "system", SnapPrefix(id))
	// Direct dependent: a volume launched from the admin import.
	putVolume(t, store, "vol-aaa", SnapPrefix(id))

	_, err := RemoveSystemImage(t.Context(), store, provider, testRemoveBucket, RemoveImageOpts{ImageID: id})
	require.Error(t, err)
	var depErr *DependentError
	require.ErrorAs(t, err, &depErr)
	assert.Empty(t, provider.deletes)
	assert.Contains(t, depErr.Dependents.Volumes, "vol-aaa")
}

func TestRemoveSystemImage_DependentVolume_Transitive_Refused(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	provider := newRemoveProvider()
	const id = "ami-sys-deb13"
	putAMI(t, store, id, "debian-13", "system", SnapPrefix(id))

	// CopyImage of the system AMI wrote a snap whose VolumeID points back at the AMI ID.
	const derivedSnap = "snap-derived-001"
	putSnapMetadata(t, store, derivedSnap, id)
	// A volume launched from the copied AMI references the derived snap.
	putVolume(t, store, "vol-bbb", derivedSnap)

	_, err := RemoveSystemImage(t.Context(), store, provider, testRemoveBucket, RemoveImageOpts{ImageID: id})
	require.Error(t, err)
	var depErr *DependentError
	require.ErrorAs(t, err, &depErr)
	assert.Empty(t, provider.deletes)
	assert.Contains(t, depErr.Dependents.Volumes, "vol-bbb")
	assert.Contains(t, depErr.Dependents.Snapshots, derivedSnap)
}

func TestRemoveSystemImage_DependentAMI_Refused(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	provider := newRemoveProvider()
	const id = "ami-sys-deb13"
	putAMI(t, store, id, "debian-13", "system", SnapPrefix(id))

	const derivedSnap = "snap-derived-002"
	putSnapMetadata(t, store, derivedSnap, id)
	// Account AMI created via CopyImage; its SnapshotID is the derived snap.
	putAMI(t, store, "ami-acct-002", "copied", testRemoveAccountID, derivedSnap)

	_, err := RemoveSystemImage(t.Context(), store, provider, testRemoveBucket, RemoveImageOpts{ImageID: id})
	require.Error(t, err)
	var depErr *DependentError
	require.ErrorAs(t, err, &depErr)
	assert.Empty(t, provider.deletes)
	assert.Contains(t, depErr.Dependents.AMIs, "ami-acct-002")
}

func TestRemoveSystemImage_Force_OverridesDependents(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	provider := newRemoveProvider()
	const id = "ami-sys-deb13"
	putAMI(t, store, id, "debian-13", "system", SnapPrefix(id))
	putImageStorage(t, provider, id, 1<<30)
	putVolume(t, store, "vol-orphan", SnapPrefix(id))

	_, err := RemoveSystemImage(t.Context(), store, provider, testRemoveBucket, RemoveImageOpts{ImageID: id, Force: true})
	require.NoError(t, err)
	assertImageStorageGone(t, provider, id)
	// The dependent volume's document remains; the AMI's is gone.
	metadata := ebsmetadata.NewStore(store, testRemoveBucket)
	_, err = metadata.GetAMI(t.Context(), id)
	require.Error(t, err)
	_, err = metadata.GetVolume(t.Context(), testRemoveAccountID, "vol-orphan")
	require.NoError(t, err)
}

func TestRemoveSystemImage_Salvage_MissingConfig_ForceCleans(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	provider := newRemoveProvider()
	const id = "ami-salvage-1"
	// No AMI document: only the provider's volume and snapshot.
	putImageStorage(t, provider, id, 1<<30)

	// Without --force: NotFound.
	_, err := RemoveSystemImage(t.Context(), store, provider, testRemoveBucket, RemoveImageOpts{ImageID: id})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())

	// With --force: salvage proceeds.
	_, err = RemoveSystemImage(t.Context(), store, provider, testRemoveBucket, RemoveImageOpts{ImageID: id, Force: true})
	require.NoError(t, err)
	assert.Equal(t, wantDeletes(id), provider.deletes)
	assertImageStorageGone(t, provider, id)
}

func TestRemoveSystemImage_Salvage_CorruptConfig_ForceCleans(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	provider := newRemoveProvider()
	const id = "ami-salvage-2"
	putCorruptAMI(t, store, id)
	putImageStorage(t, provider, id, 1<<30)

	// Without --force.
	_, err := RemoveSystemImage(t.Context(), store, provider, testRemoveBucket, RemoveImageOpts{ImageID: id})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())

	// With --force: the corrupt document and the provider storage all go.
	_, err = RemoveSystemImage(t.Context(), store, provider, testRemoveBucket, RemoveImageOpts{ImageID: id, Force: true})
	require.NoError(t, err)
	assertImageStorageGone(t, provider, id)
	assert.Equal(t, 0, store.Count())
}

func TestRemoveSystemImage_IdempotentRerun(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	provider := newRemoveProvider()
	const id = "ami-rerun"
	putAMI(t, store, id, "debian-13", "system", SnapPrefix(id))
	putImageStorage(t, provider, id, 1<<30)

	_, err := RemoveSystemImage(t.Context(), store, provider, testRemoveBucket, RemoveImageOpts{ImageID: id})
	require.NoError(t, err)
	require.Equal(t, 0, store.Count())
	assertImageStorageGone(t, provider, id)

	// Second call without --force should report NotFound.
	_, err = RemoveSystemImage(t.Context(), store, provider, testRemoveBucket, RemoveImageOpts{ImageID: id})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())

	// Salvage re-run asks the provider again and succeeds on absent targets.
	_, err = RemoveSystemImage(t.Context(), store, provider, testRemoveBucket, RemoveImageOpts{ImageID: id, Force: true})
	require.NoError(t, err)
	assert.Equal(t, append(wantDeletes(id), wantDeletes(id)...), provider.deletes)
}

func TestRemoveSystemImage_MalformedID(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	_, err := RemoveSystemImage(t.Context(), store, newRemoveProvider(), testRemoveBucket, RemoveImageOpts{ImageID: "vol-wrong"})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDMalformed, err.Error())
}

func TestPreviewRemoveSystemImage_Salvage(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	provider := newRemoveProvider()
	const id = "ami-salvage-preview"
	putImageStorage(t, provider, id, 2<<30)

	preview, err := PreviewRemoveSystemImage(t.Context(), store, provider, testRemoveBucket, id)
	require.NoError(t, err)
	assert.False(t, preview.ConfigPresent)
	assert.False(t, preview.ConfigCorrupt)
	assert.Equal(t, VolumePresent, preview.VolumeStatus)
	assert.Equal(t, int64(2<<30), preview.VolumeCapacityBytes)
}

// A provider answering not_found is an absent volume, not an unknown one.
func TestPreviewRemoveSystemImage_AbsentVolume(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	provider := newRemoveProvider()
	const id = "ami-sys-novolume"
	putAMI(t, store, id, "debian-13", "system", SnapPrefix(id))

	preview, err := PreviewRemoveSystemImage(t.Context(), store, provider, testRemoveBucket, id)
	require.NoError(t, err)
	assert.Equal(t, VolumeAbsent, preview.VolumeStatus)
	require.NoError(t, preview.VolumeErr)
}

func TestPreviewRemoveSystemImage_CorruptConfig(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const id = "ami-corrupt-preview"
	putCorruptAMI(t, store, id)

	preview, err := PreviewRemoveSystemImage(t.Context(), store, newRemoveProvider(), testRemoveBucket, id)
	require.NoError(t, err)
	assert.False(t, preview.ConfigPresent)
	assert.True(t, preview.ConfigCorrupt)
}

func TestFindAMIDependents_SkipsTargetAMI(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const id = "ami-sys-only"
	putAMI(t, store, id, "debian-13", "system", SnapPrefix(id))

	deps, err := FindAMIDependents(store, testRemoveBucket, id)
	require.NoError(t, err)
	assert.True(t, deps.Empty())
}

// putUEFIStore writes one object under imageID's UEFI variable store prefix.
func putUEFIStore(t *testing.T, store objectstore.ObjectStore, imageID string) string {
	t.Helper()
	key := uefistore.KeyPrefix(imageID) + "vars"
	_, err := store.PutObject(t.Context(), &awss3.PutObjectInput{
		Bucket: aws.String(testRemoveBucket), Key: aws.String(key), Body: bytes.NewReader([]byte("vars")),
	})
	require.NoError(t, err)
	return key
}

// RemoveSystemImage deletes the image's UEFI variable store and no other
// image's.
func TestRemoveSystemImage_DeletesOnlyItsOwnUEFIStore(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const id, other = "ami-sys-deb13", "ami-sys-deb13x"
	putAMI(t, store, id, "debian-13", "system", SnapPrefix(id))
	putUEFIStore(t, store, id)
	otherKey := putUEFIStore(t, store, other)

	res, err := RemoveSystemImage(t.Context(), store, newRemoveProvider(), testRemoveBucket, RemoveImageOpts{ImageID: id})
	require.NoError(t, err)
	assert.Equal(t, 1, res.UEFIObjectsDeleted)

	left, err := uefistore.ListObjects(t.Context(), store, testRemoveBucket, id)
	require.NoError(t, err)
	assert.Empty(t, left)
	kept, err := uefistore.ListObjects(t.Context(), store, testRemoveBucket, other)
	require.NoError(t, err)
	assert.Equal(t, []string{otherKey}, kept)
}

// failingUEFIDeletes fails every delete under the UEFI root.
type failingUEFIDeletes struct{ *objectstore.MemoryObjectStore }

func (s failingUEFIDeletes) DeleteObject(ctx context.Context, in *awss3.DeleteObjectInput) (*awss3.DeleteObjectOutput, error) {
	if strings.HasPrefix(aws.StringValue(in.Key), uefistore.ObjectRoot) {
		return nil, assert.AnError
	}
	return s.MemoryObjectStore.DeleteObject(ctx, in)
}

// The store goes before the document, so a failed delete keeps the image for
// a rerun to finish.
func TestRemoveSystemImage_FailedUEFIStoreDeleteKeepsTheDocument(t *testing.T) {
	mem := objectstore.NewMemoryObjectStore()
	const id = "ami-sys-deb13"
	putAMI(t, mem, id, "debian-13", "system", SnapPrefix(id))
	putUEFIStore(t, mem, id)

	provider := newRemoveProvider()
	_, err := RemoveSystemImage(t.Context(), failingUEFIDeletes{mem}, provider, testRemoveBucket, RemoveImageOpts{ImageID: id})

	require.Error(t, err)
	assert.Empty(t, provider.deletes)
	_, err = ebsmetadata.NewStore(mem, testRemoveBucket).GetAMI(t.Context(), id)
	require.NoError(t, err, "the document stays for the rerun")
}
