package admin

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// putAMIDocument writes an ebsmetadata AMI document directly, with no
// ami-<id>/config.json — the provider path's only representation.
func putAMIDocument(t *testing.T, store *objectstore.MemoryObjectStore, imageID, name, owner string) {
	t.Helper()
	err := ebsmetadata.NewStore(store, testRemoveBucket).PutAMI(t.Context(), ebsmetadata.AMI{
		ImageID:         imageID,
		Name:            name,
		ImageOwnerAlias: owner,
		VolumeSizeGiB:   8,
	})
	require.NoError(t, err)
}

// TestGetAMIMetadata_Document locks that GetAMIMetadata resolves an AMI that
// exists only as an ebsmetadata document, not just legacy config.json.
func TestGetAMIMetadata_Document(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	putAMIDocument(t, store, "ami-doc001", "doc-ami", testRemoveAccountID)

	meta, err := GetAMIMetadata(store, testRemoveBucket, "ami-doc001")
	require.NoError(t, err)
	assert.Equal(t, "doc-ami", meta.Name)
	assert.Equal(t, testRemoveAccountID, meta.ImageOwnerAlias)
}

// TestPromoteSystemImage_Document locks that promoting a document-only AMI
// writes back to the ebsmetadata document, not config.json.
func TestPromoteSystemImage_Document(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	putAMIDocument(t, store, "ami-doc002", "doc-ami-2", testRemoveAccountID)

	_, err := PromoteSystemImage(store, testRemoveBucket, PromoteImageOpts{ImageID: "ami-doc002"})
	require.NoError(t, err)

	doc, err := ebsmetadata.NewStore(store, testRemoveBucket).GetAMI(context.Background(), "ami-doc002")
	require.NoError(t, err)
	assert.Equal(t, SystemOwnerAlias, doc.ImageOwnerAlias)

	_, cfgErr := readAMIConfig(store, testRemoveBucket, "ami-doc002")
	assert.True(t, objectstore.IsNoSuchKeyError(cfgErr), "promotion of a document-only AMI must not write config.json")
}

// TestRemoveSystemImage_Document locks that RemoveSystemImage cleans up a
// document-only AMI's ebsmetadata document.
func TestRemoveSystemImage_Document(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	putAMIDocument(t, store, "ami-doc003", "doc-ami-3", "system")

	_, err := RemoveSystemImage(store, testRemoveBucket, RemoveImageOpts{ImageID: "ami-doc003"})
	require.NoError(t, err)

	_, err = ebsmetadata.NewStore(store, testRemoveBucket).GetAMI(context.Background(), "ami-doc003")
	assert.True(t, objectstore.IsNoSuchKeyError(err), "ebsmetadata document must be gone after removal")
}

// TestFindAMIDependents_DocumentOnlyVolume locks that a provider-managed
// volume (an ebsmetadata document only, no vol-<id>/config.json) is still
// discoverable as a dependent through the ebsmetadata ListVolumes union.
//
// There is no equivalent "document-only snapshot" case: EC2 snapshots have
// no ebsmetadata document type at all (see ebsmetadata/metadata.go) and
// SnapshotServiceImpl.CreateSnapshot writes {snapshotID}/metadata.json
// unconditionally on both the provider and embedded paths, so pass 1's
// existing prefix scan already sees every snapshot regardless of provider.
func TestFindAMIDependents_DocumentOnlyVolume(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const src = "ami-doc-src-2"
	putAMI(t, store, src, "src-ami-2", "system", SnapPrefix(src))

	err := ebsmetadata.NewStore(store, testRemoveBucket).PutVolume(context.Background(), ebsmetadata.Volume{
		VolumeID:   "vol-doc001",
		SnapshotID: SnapPrefix(src),
	})
	require.NoError(t, err)

	deps, err := FindAMIDependents(store, testRemoveBucket, src)
	require.NoError(t, err)
	assert.Contains(t, deps.Volumes, "vol-doc001")
}

// TestFindAMIDependents_DocumentOnlyAMI locks that a document-only AMI
// (registered via CopyImage under the provider, e.g.) is still discoverable
// as a dependent through the ebsmetadata ListAMIs union, not just the
// ami-<id>/ prefix scan.
func TestFindAMIDependents_DocumentOnlyAMI(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const src = "ami-doc-src"
	putAMI(t, store, src, "src-ami", "system", SnapPrefix(src))

	// A CopyImage-derived AMI that only exists as an ebsmetadata document,
	// referencing a snap derived from src.
	derivedSnap := "snap-derived-001"
	putSnapMetadata(t, store, derivedSnap, src)
	err := ebsmetadata.NewStore(store, testRemoveBucket).PutAMI(context.Background(), ebsmetadata.AMI{
		ImageID:         "ami-doc-derived",
		Name:            "derived-ami",
		ImageOwnerAlias: "system",
		SnapshotID:      derivedSnap,
	})
	require.NoError(t, err)

	deps, err := FindAMIDependents(store, testRemoveBucket, src)
	require.NoError(t, err)
	assert.Contains(t, deps.AMIs, "ami-doc-derived")
}
