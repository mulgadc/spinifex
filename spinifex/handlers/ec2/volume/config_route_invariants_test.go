package handlers_ec2_volume

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/nats-io/nats.go"
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
		VolumeConfig: viperblock.VolumeConfig{
			VolumeMetadata: viperblock.VolumeMetadata{VolumeID: volumeID, SizeGiB: 10, State: "available"},
		},
	}
	data, err := json.Marshal(state)
	require.NoError(t, err)
	_, err = store.PutObject(&s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(volumeID + "/config.json"),
		Body:   strings.NewReader(string(data)),
	})
	require.NoError(t, err)
}

// TestUpdateVolumeState_MountedRoutesToLiveVB locks the single-writer contract
// (mulga-siv-466): while a volume is mounted (a node answers
// ebs.config.{volumeID}), every control-plane state update MUST be routed to
// that live VB, never written to config.json out-of-band. The live mounted VB
// owns the volume's StateSeqNum and its SaveState authoritatively rewrites
// config.json from in-memory; an out-of-band object write is silently clobbered
// by the next SaveState (and, for an encrypted volume, opening a second writer
// reuses the AES-GCM nonce — catastrophic). This must hold for unencrypted
// volumes too: the routing decision cannot depend on the racy on-disk
// encryption check, only on whether a live owner exists.
func TestUpdateVolumeState_MountedRoutesToLiveVB(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	svc.natsConn = startTestNATS(t)

	volumeID := "vol-plain-live"
	seedUnencryptedConfig(t, store, volumeID)
	before := getStoredConfig(t, store, volumeID)

	var got atomic.Pointer[types.EBSConfigUpdateRequest]
	sub, err := svc.natsConn.Subscribe("ebs.config."+volumeID, func(msg *nats.Msg) {
		var req types.EBSConfigUpdateRequest
		_ = json.Unmarshal(msg.Data, &req)
		got.Store(&req)
		data, _ := json.Marshal(types.EBSConfigUpdateResponse{Volume: volumeID, Success: true})
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	err = svc.UpdateVolumeState(volumeID, "in-use", "i-abc", "/dev/nbd0")
	require.NoError(t, err)

	req := got.Load()
	require.NotNil(t, req,
		"mulga-siv-466: a mounted volume's state update MUST route to the live VB (ebs.config.{volumeID}), not an out-of-band object write")
	var vc viperblock.VolumeConfig
	require.NoError(t, json.Unmarshal(req.VolumeConfig, &vc))
	assert.Equal(t, "in-use", vc.VolumeMetadata.State)
	assert.Equal(t, "i-abc", vc.VolumeMetadata.AttachedInstance)

	// The handler must NOT have rewritten the object: the live VB owns it and
	// its SaveState would otherwise clobber the control-plane update.
	after := getStoredConfig(t, store, volumeID)
	assert.Equal(t, string(before), string(after),
		"mulga-siv-466: handler must not write config.json out-of-band while a live VB owns the volume")
}

// TestUpdateVolumeState_DetachedUnencryptedWritesObject confirms the fallback is
// preserved: with no live owner, an unencrypted update writes the object
// directly (the live VB cannot be clobbered because there is none).
func TestUpdateVolumeState_DetachedUnencryptedWritesObject(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	svc.natsConn = startTestNATS(t)

	volumeID := "vol-plain-detached"
	seedUnencryptedConfig(t, store, volumeID)

	// No ebs.config.* responder: the per-volume request returns ErrNoResponders
	// and the handler falls back to the direct object write.
	err := svc.UpdateVolumeState(volumeID, "in-use", "i-xyz", "/dev/nbd1")
	require.NoError(t, err)

	raw := getStoredConfig(t, store, volumeID)
	var state viperblock.VBState
	require.NoError(t, json.Unmarshal(viperblock.StateBody(raw), &state))
	assert.Equal(t, "in-use", state.VolumeConfig.VolumeMetadata.State)
	assert.Equal(t, "i-xyz", state.VolumeConfig.VolumeMetadata.AttachedInstance)
}
