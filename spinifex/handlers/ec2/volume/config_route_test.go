package handlers_ec2_volume

import (
	"encoding/json"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startTestNATS spins up an in-process NATS server and returns a connected client.
func startTestNATS(t *testing.T) *nats.Conn {
	t.Helper()
	opts := &server.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	}
	ns, err := server.NewServer(opts)
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second))
	t.Cleanup(func() { ns.Shutdown() })

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(func() { nc.Close() })
	return nc
}

// seedEncryptedConfig writes a sealed (EncryptionEnabled=true) VBState to the store.
func seedEncryptedConfig(t *testing.T, store *objectstore.MemoryObjectStore, volumeID string) {
	t.Helper()
	state := viperblock.VBState{
		VolumeName:        volumeID,
		VolumeSize:        10 * 1024 * 1024 * 1024,
		BlockSize:         4096,
		EncryptionEnabled: true,
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

// getStoredConfig reads the raw config.json bytes from the memory store.
func getStoredConfig(t *testing.T, store *objectstore.MemoryObjectStore, volumeID string) []byte {
	t.Helper()
	res, err := store.GetObject(&s3.GetObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(volumeID + "/config.json"),
	})
	require.NoError(t, err)
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return data
}

// TestUpdateVolumeState_EncryptedRoutesToLiveVB verifies an encrypted-volume
// state update is delegated to the owning node via ebs.config.{volumeID} rather
// than rewriting the sealed object directly.
func TestUpdateVolumeState_EncryptedRoutesToLiveVB(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	svc.natsConn = startTestNATS(t)

	volumeID := "vol-enc-live"
	seedEncryptedConfig(t, store, volumeID)

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
	require.NotNil(t, req, "per-volume responder must receive the config update")
	var vc viperblock.VolumeConfig
	require.NoError(t, json.Unmarshal(req.VolumeConfig, &vc))
	assert.Equal(t, "in-use", vc.VolumeMetadata.State)
	assert.Equal(t, "i-abc", vc.VolumeMetadata.AttachedInstance)
	assert.Equal(t, "/dev/nbd0", vc.VolumeMetadata.DeviceName)

	// The sealed object must NOT have been rewritten by the handler.
	raw := getStoredConfig(t, store, volumeID)
	var still viperblock.VBState
	require.NoError(t, json.Unmarshal(viperblock.StateBody(raw), &still))
	assert.True(t, still.EncryptionEnabled)
	assert.Equal(t, "available", still.VolumeConfig.VolumeMetadata.State,
		"handler must not mutate the sealed object directly; the keyholder owns it")
}

// TestUpdateVolumeState_EncryptedDetachedFallsBackToQueueGroup verifies that with
// no per-volume responder (volume unmounted), the update routes to the ebs.config
// queue group.
func TestUpdateVolumeState_EncryptedDetachedFallsBackToQueueGroup(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	svc.natsConn = startTestNATS(t)

	volumeID := "vol-enc-detached"
	seedEncryptedConfig(t, store, volumeID)

	var hits atomic.Int32
	sub, err := svc.natsConn.QueueSubscribe("ebs.config", "spinifex-workers", func(msg *nats.Msg) {
		hits.Add(1)
		data, _ := json.Marshal(types.EBSConfigUpdateResponse{Volume: volumeID, Success: true})
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	err = svc.UpdateVolumeState(volumeID, "available", "", "")
	require.NoError(t, err)
	assert.Equal(t, int32(1), hits.Load(), "detached encrypted update must hit the ebs.config queue group")
}

// TestUpdateVolumeState_EncryptedKeyholderError surfaces a keyholder failure
// instead of silently succeeding.
func TestUpdateVolumeState_EncryptedKeyholderError(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	svc.natsConn = startTestNATS(t)

	volumeID := "vol-enc-err"
	seedEncryptedConfig(t, store, volumeID)

	sub, err := svc.natsConn.Subscribe("ebs.config."+volumeID, func(msg *nats.Msg) {
		data, _ := json.Marshal(types.EBSConfigUpdateResponse{Volume: volumeID, Error: "reseal failed"})
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	err = svc.UpdateVolumeState(volumeID, "in-use", "i-x", "/dev/nbd0")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reseal failed")
}
