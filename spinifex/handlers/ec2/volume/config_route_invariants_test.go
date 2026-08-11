package handlers_ec2_volume

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUpdateVolumeState_WritesDocumentNotConfig locks the single-writer
// contract: the live nbdkit VB owns config.json and rewrites it from its stale
// in-memory State on every SaveState, so the control plane MUST persist
// attachment state to its own document and leave config.json byte-for-byte
// untouched. Writing State into config.json out-of-band is silently clobbered
// under write load (the EKS-worker root-volume flip).
func TestUpdateVolumeState_WritesDocumentNotConfig(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	volumeID := "vol-single-writer"
	seedProviderConfig(t, store, volumeID)
	seedVolumeDocument(t, store, ebsmetadata.Volume{
		VolumeID: volumeID, TenantID: testVolAccountID, CapacityGiB: 10, State: "available",
	})
	before := getStoredConfig(t, store, volumeID)

	require.NoError(t, svc.UpdateVolumeState(volumeID, "in-use", "i-abc", "/dev/nbd0"))

	after := getStoredConfig(t, store, volumeID)
	assert.Equal(t, string(before), string(after),
		"UpdateVolumeState must not write config.json; the live VB owns it")

	meta, err := svc.GetVolumeMetadata(volumeID)
	require.NoError(t, err)
	assert.Equal(t, "in-use", meta.State)
	assert.Equal(t, "i-abc", meta.AttachedInstance)
	assert.Equal(t, "/dev/nbd0", meta.DeviceName)
}

// TestUpdateVolumeState_EncryptedConfigUntouched locks the corruption-safety
// half of the contract: config.json for an encrypted volume is a sealed blob
// whose AES-GCM nonce is derived from its sequence number. A second out-of-band
// writer advances the nonce and reuses it (catastrophic for AES-GCM — the
// decrypted garbage image layers). UpdateVolumeState MUST never touch it.
func TestUpdateVolumeState_EncryptedConfigUntouched(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	volumeID := "vol-enc-single-writer"
	seedEncryptedConfig(t, store, volumeID)
	seedVolumeDocument(t, store, ebsmetadata.Volume{
		VolumeID: volumeID, TenantID: testVolAccountID, CapacityGiB: 10, State: "available",
	})
	before := getStoredConfig(t, store, volumeID)

	require.NoError(t, svc.UpdateVolumeState(volumeID, "in-use", "i-enc", "/dev/nbd0"))

	after := getStoredConfig(t, store, volumeID)
	assert.Equal(t, string(before), string(after),
		"UpdateVolumeState must not rewrite the sealed config.json (AES-GCM nonce reuse)")

	meta, err := svc.GetVolumeMetadata(volumeID)
	require.NoError(t, err)
	assert.Equal(t, "in-use", meta.State)
}

// TestGetVolumeMetadata_DocumentBeatsConfig locks the read side of the same
// contract: config.json carries a State the provider wrote and the control
// plane does not own, so a document that disagrees with it MUST win. Reading
// State back out of config.json is what let a stale SaveState resurrect a
// detached volume as attached.
func TestGetVolumeMetadata_DocumentBeatsConfig(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	volumeID := "vol-document-wins"
	seedProviderConfig(t, store, volumeID) // its embedded State says "available"
	seedVolumeDocument(t, store, ebsmetadata.Volume{
		VolumeID: volumeID, TenantID: testVolAccountID, CapacityGiB: 10,
		State: "in-use", AttachedInstance: "i-overlay", DeviceName: "/dev/nbd0",
	})

	meta, err := svc.GetVolumeMetadata(volumeID)
	require.NoError(t, err)
	assert.Equal(t, "in-use", meta.State,
		"the ebsmetadata document must override any State in config.json")
	assert.Equal(t, "i-overlay", meta.AttachedInstance)
	assert.Equal(t, "/dev/nbd0", meta.DeviceName)
}

// TestGetVolumeMetadata_ConfigAloneIsNotAVolume is the other half: provider
// state without a document is not a volume the control plane knows. Falling
// back to config.json would resurrect volumes whose document was deleted, so
// an absent document must read as not-found.
func TestGetVolumeMetadata_ConfigAloneIsNotAVolume(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	volumeID := "vol-config-only"
	seedProviderConfig(t, store, volumeID)

	_, err := svc.GetVolumeMetadata(volumeID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidVolume.NotFound")
}
