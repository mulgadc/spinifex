package handlers_ec2_volume

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// TestUpdateVolumeState_MountedRoutesToLiveVB locks the single-writer contract:
// while a volume is mounted (a node answers
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
		"a mounted volume's state update MUST route to the live VB (ebs.config.{volumeID}), not an out-of-band object write")
	var vc viperblock.VolumeConfig
	require.NoError(t, json.Unmarshal(req.VolumeConfig, &vc))
	assert.Equal(t, "in-use", vc.VolumeMetadata.State)
	assert.Equal(t, "i-abc", vc.VolumeMetadata.AttachedInstance)

	// The handler must NOT have rewritten the object: the live VB owns it and
	// its SaveState would otherwise clobber the control-plane update.
	after := getStoredConfig(t, store, volumeID)
	assert.Equal(t, string(before), string(after),
		"handler must not write config.json out-of-band while a live VB owns the volume")
}

// TestUpdateVolumeState_DetachedUnencryptedWritesObject confirms the fallback is
// preserved: detaching a volume (state available) has no live owner, so the
// unencrypted update writes the object directly — there is no live VB to clobber.
func TestUpdateVolumeState_DetachedUnencryptedWritesObject(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	svc.natsConn = startTestNATS(t)

	volumeID := "vol-plain-detached"
	seedUnencryptedConfig(t, store, volumeID)

	// Detach (available): not "attaching", so no per-volume responder is expected
	// and the handler falls back to the direct object write immediately.
	err := svc.UpdateVolumeState(volumeID, "available", "", "")
	require.NoError(t, err)

	raw := getStoredConfig(t, store, volumeID)
	var state viperblock.VBState
	require.NoError(t, json.Unmarshal(viperblock.StateBody(raw), &state))
	assert.Equal(t, "available", state.VolumeConfig.VolumeMetadata.State)
	assert.Equal(t, "", state.VolumeConfig.VolumeMetadata.AttachedInstance)
}

// TestUpdateVolumeState_InUseRetriesUntilLiveVBAppears locks the race fix: an
// in-use (attach) mark can fire seconds before the mount's
// ebs.config.{volumeID} responder propagates. The handler MUST keep retrying the
// live-VB route rather than fall back to an out-of-band object write that the
// live VB's SaveState then silently clobbers. Here the responder is registered
// shortly after the mark begins; the update must still reach the live VB and the
// object must NOT be rewritten.
func TestUpdateVolumeState_InUseRetriesUntilLiveVBAppears(t *testing.T) {
	oldWait, oldRetry := liveVBAttachWait, liveVBAttachRetry
	liveVBAttachWait, liveVBAttachRetry = 5*time.Second, 50*time.Millisecond
	defer func() { liveVBAttachWait, liveVBAttachRetry = oldWait, oldRetry }()

	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	svc.natsConn = startTestNATS(t)

	volumeID := "vol-race"
	seedEncryptedConfig(t, store, volumeID)
	before := getStoredConfig(t, store, volumeID)

	// Responder comes up 300ms late — after the in-use mark has started and hit
	// ErrNoResponders at least once.
	var got atomic.Bool
	go func() {
		time.Sleep(300 * time.Millisecond)
		sub, _ := svc.natsConn.Subscribe("ebs.config."+volumeID, func(msg *nats.Msg) {
			got.Store(true)
			data, _ := json.Marshal(types.EBSConfigUpdateResponse{Volume: volumeID, Success: true})
			_ = msg.Respond(data)
		})
		_ = svc.natsConn.Flush()
		t.Cleanup(func() { sub.Unsubscribe() })
	}()

	err := svc.UpdateVolumeState(volumeID, "in-use", "i-race", "/dev/nbd0")
	require.NoError(t, err)
	assert.True(t, got.Load(),
		"in-use mark must retry the live VB until the late mount responder appears, not fall back to an out-of-band write")

	after := getStoredConfig(t, store, volumeID)
	assert.Equal(t, string(before), string(after),
		"the sealed object must not be rewritten out-of-band while the volume is (becoming) mounted")
}
