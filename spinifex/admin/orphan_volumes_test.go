package admin

import (
	"bytes"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const orphanTestBucket = "test-bucket"

func newOrphanScanFixture(t *testing.T) (*ebsprovider.MemoryProvider, *ebsmetadata.Store) {
	t.Helper()
	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{VolumeEnumeration: true, VolumeSeeding: true})
	return provider, ebsmetadata.NewStore(objectstore.NewMemoryObjectStore(), orphanTestBucket)
}

// createProviderVolume gives the provider blocks without telling the control
// plane, which is what a volume looks like after its document is lost.
func createProviderVolume(t *testing.T, provider *ebsprovider.MemoryProvider, volumeID string) {
	t.Helper()
	_, err := provider.CreateVolume(t.Context(), ebsprovider.CreateVolumeRequest{
		Versioned:     ebsprovider.NewVersioned(),
		VolumeID:      volumeID,
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
	})
	require.NoError(t, err)
}

func documentVolume(t *testing.T, metadata *ebsmetadata.Store, volumeID string) {
	t.Helper()
	require.NoError(t, metadata.PutVolume(t.Context(), ebsmetadata.Volume{
		VolumeID: volumeID, TenantID: "123456789012", CapacityGiB: 1, State: "available",
	}))
}

// putRawVolumeDocument writes bytes straight to a volume's document key,
// so a test can plant one the store cannot decode.
func putRawVolumeDocument(t *testing.T, store objectstore.ObjectStore, volumeID string, body []byte) {
	t.Helper()
	key, err := ebsmetadata.VolumeKey(volumeID)
	require.NoError(t, err)
	_, err = store.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String(orphanTestBucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(body),
	})
	require.NoError(t, err)
}

// TestFindOrphanVolumes_FindsAVolumeWhoseDocumentIsGone is the reason the
// enumeration verb exists. Deleting the document is exactly the failure the
// consolidated boundary made unrecoverable: the volume vanishes from
// DescribeVolumes and from the leak reaper, both of which read documents.
func TestFindOrphanVolumes_FindsAVolumeWhoseDocumentIsGone(t *testing.T) {
	provider, metadata := newOrphanScanFixture(t)
	createProviderVolume(t, provider, "vol-documented000")
	documentVolume(t, metadata, "vol-documented000")
	createProviderVolume(t, provider, "vol-stranded00000")

	orphans, err := FindOrphanVolumes(t.Context(), provider, metadata)
	require.NoError(t, err)

	require.Len(t, orphans, 1, "the scan must report exactly the volume with no document")
	assert.Equal(t, "vol-stranded00000", orphans[0].VolumeID,
		"a volume the provider holds with no control-plane document is unreachable through EC2 and must be surfaced")
	assert.False(t, orphans[0].Derived)
}

// TestFindOrphanVolumes_NeverDeletes locks ADR-0005 §3 for this tool: an
// orphan carries data, and the evidence it is still wanted is the very
// document that is missing. The scan may only report.
func TestFindOrphanVolumes_NeverDeletes(t *testing.T) {
	provider, metadata := newOrphanScanFixture(t)
	createProviderVolume(t, provider, "vol-stranded00000")

	orphans, err := FindOrphanVolumes(t.Context(), provider, metadata)
	require.NoError(t, err)
	require.Len(t, orphans, 1)

	got, err := provider.GetVolume(t.Context(), ebsprovider.GetVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-stranded00000",
	})
	require.NoErrorf(t, err, "ADR-0005 §3: the orphan scan must never delete the volume it reports")
	assert.Equal(t, "vol-stranded00000", got.ID)
}

// An EFI variable store is created without a document by design, so it is only
// an orphan when the root volume it belongs to is also undocumented.
func TestFindOrphanVolumes_DerivedVolumeExcusedByItsBase(t *testing.T) {
	provider, metadata := newOrphanScanFixture(t)
	createProviderVolume(t, provider, "vol-live00000000")
	createProviderVolume(t, provider, "vol-live00000000-efi")
	documentVolume(t, metadata, "vol-live00000000")

	orphans, err := FindOrphanVolumes(t.Context(), provider, metadata)
	require.NoError(t, err)
	assert.Empty(t, orphans, "an EFI store whose root volume is documented is expected, not orphaned")
}

func TestFindOrphanVolumes_DerivedVolumeReportedWhenItsBaseIsGone(t *testing.T) {
	provider, metadata := newOrphanScanFixture(t)
	createProviderVolume(t, provider, "vol-gone00000000")
	createProviderVolume(t, provider, "vol-gone00000000-efi")

	orphans, err := FindOrphanVolumes(t.Context(), provider, metadata)
	require.NoError(t, err)

	require.Len(t, orphans, 2, "a stranded pair must both be reported; the EFI store's lifecycle owner is gone too")
	assert.Equal(t, "vol-gone00000000", orphans[0].VolumeID)
	assert.Equal(t, "vol-gone00000000-efi", orphans[1].VolumeID)
	assert.True(t, orphans[1].Derived)
}

// An unreadable document must abort the scan, never read as absent: reporting
// a volume as orphaned because its document failed to decode would point an
// operator at live data.
func TestFindOrphanVolumes_UndecodableDocumentAbortsTheScan(t *testing.T) {
	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{VolumeEnumeration: true})
	store := objectstore.NewMemoryObjectStore()
	metadata := ebsmetadata.NewStore(store, orphanTestBucket)
	createProviderVolume(t, provider, "vol-corruptdoc00")
	putRawVolumeDocument(t, store, "vol-corruptdoc00", []byte("{not json"))

	_, err := FindOrphanVolumes(t.Context(), provider, metadata)
	require.Error(t, err, "an undecodable document must fail the scan, not be treated as a missing one")
}

func TestFindOrphanVolumes_RequiresEnumerationCapability(t *testing.T) {
	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})
	metadata := ebsmetadata.NewStore(objectstore.NewMemoryObjectStore(), orphanTestBucket)

	_, err := FindOrphanVolumes(t.Context(), provider, metadata)
	require.ErrorIs(t, err, ebsprovider.ErrUnsupportedCapability)
}

func TestFindOrphanVolumes_EmptyProviderReportsNothing(t *testing.T) {
	provider, metadata := newOrphanScanFixture(t)
	orphans, err := FindOrphanVolumes(t.Context(), provider, metadata)
	require.NoError(t, err)
	assert.Empty(t, orphans)
}
