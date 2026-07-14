package handlers_ec2_volume

import (
	"context"
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
	_, err = store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(volumeID + "/config.json"),
		Body:   strings.NewReader(string(data)),
	})
	require.NoError(t, err)
}

// getStoredConfig reads the raw config.json bytes from the memory store.
func getStoredConfig(t *testing.T, store *objectstore.MemoryObjectStore, volumeID string) []byte {
	t.Helper()
	res, err := store.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(volumeID + "/config.json"),
	})
	require.NoError(t, err)
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return data
}

// encConfig builds a VolumeConfig for an encrypted-volume putVolumeConfig call.
func encConfig(volumeID string) *viperblock.VolumeConfig {
	return &viperblock.VolumeConfig{
		VolumeMetadata: viperblock.VolumeMetadata{VolumeID: volumeID, SizeGiB: 10},
	}
}

// TestPutVolumeConfig_EncryptedRoutesToQueueGroup verifies a config write for an
// encrypted volume is delegated to the ebs.config keyholder queue group rather
// than re-sealing the object out-of-band. The control plane lacks the master key,
// so a direct PutObject would strip the AES-GCM tag and brick the volume. This is
// the safe non-live writer path (markVolumeOrphaned / ModifyVolume); UpdateVolumeState
// no longer routes here at all (it writes state.json).
func TestPutVolumeConfig_EncryptedRoutesToQueueGroup(t *testing.T) {
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

	require.NoError(t, svc.putVolumeConfig(context.Background(), volumeID, encConfig(volumeID)))
	assert.Equal(t, int32(1), hits.Load(), "encrypted config write must hit the ebs.config keyholder queue group")

	// The sealed object must NOT have been rewritten out-of-band by the handler.
	raw := getStoredConfig(t, store, volumeID)
	var still viperblock.VBState
	require.NoError(t, json.Unmarshal(viperblock.StateBody(raw), &still))
	assert.True(t, still.EncryptionEnabled)
}

// TestPutVolumeConfig_EncryptedKeyholderError surfaces a keyholder failure
// instead of silently succeeding.
func TestPutVolumeConfig_EncryptedKeyholderError(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	svc.natsConn = startTestNATS(t)

	volumeID := "vol-enc-err"
	seedEncryptedConfig(t, store, volumeID)

	sub, err := svc.natsConn.QueueSubscribe("ebs.config", "spinifex-workers", func(msg *nats.Msg) {
		data, _ := json.Marshal(types.EBSConfigUpdateResponse{Volume: volumeID, Error: "reseal failed"})
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	err = svc.putVolumeConfig(context.Background(), volumeID, encConfig(volumeID))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reseal failed")
}
