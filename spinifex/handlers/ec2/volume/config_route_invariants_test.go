package handlers_ec2_volume

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedUnencryptedConfig writes a plain (EncryptionEnabled=false) full VBState.
func seedUnencryptedConfig(t *testing.T, store *objectstore.MemoryObjectStore, volumeID string) {
	t.Helper()
	state := viperblock.VBState{
		VolumeName: volumeID,
		VolumeSize: 10 * 1024 * 1024 * 1024,
		BlockSize:  4096,
		SeqNum:     7,
		VolumeConfig: viperblock.VolumeConfig{
			VolumeMetadata: viperblock.VolumeMetadata{VolumeID: volumeID, SizeGiB: 10, State: "available"},
		},
	}
	data, err := json.Marshal(state)
	require.NoError(t, err)
	_, err = store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(volumeID + "/config.json"),
		Body:   strings.NewReader(string(data)),
	})
	require.NoError(t, err)
}

// TestUpdateVolumeState_WritesStateJSONNotConfig locks the single-writer
// contract: the live nbdkit VB owns config.json and rewrites it from its stale
// in-memory State on every SaveState, so the control plane MUST persist
// attachment state to the separate state.json object and leave config.json
// byte-for-byte untouched. Writing State into config.json out-of-band is
// silently clobbered under write load (the EKS-worker root-volume flip).
func TestUpdateVolumeState_WritesStateJSONNotConfig(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	volumeID := "vol-single-writer"
	seedUnencryptedConfig(t, store, volumeID)
	before := getStoredConfig(t, store, volumeID)

	require.NoError(t, svc.UpdateVolumeState(volumeID, "in-use", "i-abc", "/dev/nbd0"))

	after := getStoredConfig(t, store, volumeID)
	assert.Equal(t, string(before), string(after),
		"UpdateVolumeState must not write config.json; the live VB owns it")

	rec := getStoredState(t, store, volumeID)
	assert.Equal(t, "in-use", rec.State)
	assert.Equal(t, "i-abc", rec.AttachedInstance)
	assert.Equal(t, "/dev/nbd0", rec.DeviceName)
}

// TestUpdateVolumeState_EncryptedConfigUntouched locks the corruption-safety
// half of the contract: config.json for an encrypted volume is a sealed VBState
// whose AES-GCM nonce is derived from StateSeqNum. A second out-of-band writer
// advances the nonce and reuses it (catastrophic for AES-GCM — the decrypted
// garbage image layers). UpdateVolumeState MUST never touch the sealed object.
func TestUpdateVolumeState_EncryptedConfigUntouched(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	volumeID := "vol-enc-single-writer"
	seedEncryptedConfig(t, store, volumeID)
	before := getStoredConfig(t, store, volumeID)

	require.NoError(t, svc.UpdateVolumeState(volumeID, "in-use", "i-enc", "/dev/nbd0"))

	after := getStoredConfig(t, store, volumeID)
	assert.Equal(t, string(before), string(after),
		"UpdateVolumeState must not rewrite the sealed config.json (AES-GCM nonce reuse)")
	assert.Equal(t, "in-use", getStoredState(t, store, volumeID).State)
}

// TestGetVolumeConfig_OverlaysStateJSON locks the read side: the authoritative
// attachment state is state.json, which MUST override the (stale) State the live
// VB persisted into config.json.
func TestGetVolumeConfig_OverlaysStateJSON(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	volumeID := "vol-overlay"
	seedUnencryptedConfig(t, store, volumeID) // config.json State == "available"
	require.NoError(t, svc.UpdateVolumeState(volumeID, "in-use", "i-overlay", "/dev/nbd0"))

	cfg, err := svc.GetVolumeConfig(volumeID)
	require.NoError(t, err)
	assert.Equal(t, "in-use", cfg.VolumeMetadata.State,
		"state.json must override the stale State in config.json")
	assert.Equal(t, "i-overlay", cfg.VolumeMetadata.AttachedInstance)
	assert.Equal(t, "/dev/nbd0", cfg.VolumeMetadata.DeviceName)
}

// TestGetVolumeConfig_FallsBackWhenNoStateJSON locks the migration path: a volume
// predating the state.json split has no state.json, so the State embedded in
// config.json is used unchanged.
func TestGetVolumeConfig_FallsBackWhenNoStateJSON(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	volumeID := "vol-legacy"
	seedUnencryptedConfig(t, store, volumeID) // State == "available", no state.json

	cfg, err := svc.GetVolumeConfig(volumeID)
	require.NoError(t, err)
	assert.Equal(t, "available", cfg.VolumeMetadata.State,
		"absent state.json must fall back to the State embedded in config.json")
}

// getStoredState reads and decodes the raw state.json from the memory store.
func getStoredState(t *testing.T, store *objectstore.MemoryObjectStore, volumeID string) volumeStateRecord {
	t.Helper()
	res, err := store.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(volumeID + "/state.json"),
	})
	require.NoError(t, err)
	defer res.Body.Close()
	var rec volumeStateRecord
	require.NoError(t, json.NewDecoder(res.Body).Decode(&rec))
	return rec
}
