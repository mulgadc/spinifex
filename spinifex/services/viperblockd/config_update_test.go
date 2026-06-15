package viperblockd

// Tests for the encrypted-volume config-update path: applyConfigUpdate,
// makeConfigUpdateHandler (ebs.config.{volumeID}), and the ebs.config
// queue-group fallback handler in launchService.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const gib = uint64(1024 * 1024 * 1024)

// configReq marshals a VolumeConfig carrying the given size into an update request.
func configReq(t *testing.T, volume string, sizeGiB uint64) types.EBSConfigUpdateRequest {
	t.Helper()
	vc := viperblock.VolumeConfig{}
	vc.VolumeMetadata.VolumeID = volume
	vc.VolumeMetadata.SizeGiB = sizeGiB
	raw, err := json.Marshal(vc)
	require.NoError(t, err)
	return types.EBSConfigUpdateRequest{Volume: volume, VolumeConfig: raw}
}

// --- applyConfigUpdate ---

func TestApplyConfigUpdate_GrowsVolumeSize(t *testing.T) {
	t.Parallel()
	vb := createTestVBWithState(t, "vol-apply-grow")

	require.NoError(t, applyConfigUpdate(vb, configReq(t, "vol-apply-grow", 5)))

	assert.Equal(t, uint64(5), vb.VolumeConfig.VolumeMetadata.SizeGiB)
	assert.Equal(t, 5*gib, vb.VolumeSize)
}

func TestApplyConfigUpdate_ShrinkKeepsSize(t *testing.T) {
	t.Parallel()
	vb := createTestVBWithState(t, "vol-apply-shrink")
	vb.VolumeSize = 10 * gib

	require.NoError(t, applyConfigUpdate(vb, configReq(t, "vol-apply-shrink", 2)))

	// Config metadata applied, but VolumeSize is grow-only.
	assert.Equal(t, uint64(2), vb.VolumeConfig.VolumeMetadata.SizeGiB)
	assert.Equal(t, 10*gib, vb.VolumeSize)
}

func TestApplyConfigUpdate_InvalidVolumeConfig(t *testing.T) {
	t.Parallel()
	vb := createTestVBWithState(t, "vol-apply-bad")

	req := types.EBSConfigUpdateRequest{Volume: "vol-apply-bad", VolumeConfig: json.RawMessage(`"not-an-object"`)}
	err := applyConfigUpdate(vb, req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unmarshal VolumeConfig")
}

// --- makeConfigUpdateHandler (ebs.config.{volumeID}) ---

func TestConfigUpdateHandler_Success(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	vb := createTestVBWithState(t, "vol-cfg-ok")
	sub, err := nc.Subscribe("ebs.config.vol-cfg-ok", makeConfigUpdateHandler(vb, "vol-cfg-ok"))
	require.NoError(t, err)
	defer sub.Unsubscribe()
	nc.Flush()

	reqData, _ := json.Marshal(configReq(t, "vol-cfg-ok", 7))
	msg, err := nc.Request("ebs.config.vol-cfg-ok", reqData, 3*time.Second)
	require.NoError(t, err)

	var resp types.EBSConfigUpdateResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	assert.True(t, resp.Success)
	assert.Empty(t, resp.Error)
	assert.Equal(t, uint64(7), vb.VolumeConfig.VolumeMetadata.SizeGiB)
}

func TestConfigUpdateHandler_InvalidJSON(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	vb := createTestVBWithState(t, "vol-cfg-badjson")
	sub, err := nc.Subscribe("ebs.config.vol-cfg-badjson", makeConfigUpdateHandler(vb, "vol-cfg-badjson"))
	require.NoError(t, err)
	defer sub.Unsubscribe()
	nc.Flush()

	msg, err := nc.Request("ebs.config.vol-cfg-badjson", []byte("not json {{{"), 3*time.Second)
	require.NoError(t, err)

	var resp types.EBSConfigUpdateResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	assert.False(t, resp.Success)
	assert.Contains(t, resp.Error, "bad request:")
}

func TestConfigUpdateHandler_ApplyError(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	vb := createTestVBWithState(t, "vol-cfg-applyerr")
	sub, err := nc.Subscribe("ebs.config.vol-cfg-applyerr", makeConfigUpdateHandler(vb, "vol-cfg-applyerr"))
	require.NoError(t, err)
	defer sub.Unsubscribe()
	nc.Flush()

	reqData, _ := json.Marshal(types.EBSConfigUpdateRequest{
		Volume:       "vol-cfg-applyerr",
		VolumeConfig: json.RawMessage(`"not-an-object"`),
	})
	msg, err := nc.Request("ebs.config.vol-cfg-applyerr", reqData, 3*time.Second)
	require.NoError(t, err)

	var resp types.EBSConfigUpdateResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	assert.False(t, resp.Success)
	assert.Contains(t, resp.Error, "unmarshal VolumeConfig")
}

// --- ebs.config queue-group fallback ---

func TestEBSConfigQueueGroup_LiveVB(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	vb := createTestVBWithState(t, "vol-qg-live")
	cfg := setupTestConfig(t, natsURL)
	cfg.MountedVolumes = []MountedVolume{{Name: "vol-qg-live", VB: vb}}

	go func() { launchService(cfg) }()
	time.Sleep(500 * time.Millisecond)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	reqData, _ := json.Marshal(configReq(t, "vol-qg-live", 9))
	msg, err := nc.Request("ebs.config", reqData, 3*time.Second)
	require.NoError(t, err)

	var resp types.EBSConfigUpdateResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	assert.True(t, resp.Success)
	assert.Empty(t, resp.Error)
	assert.Equal(t, uint64(9), vb.VolumeConfig.VolumeMetadata.SizeGiB)
}

func TestEBSConfigQueueGroup_InvalidJSON(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	cfg := setupTestConfig(t, natsURL)

	go func() { launchService(cfg) }()
	time.Sleep(500 * time.Millisecond)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	msg, err := nc.Request("ebs.config", []byte("not json {{{"), 3*time.Second)
	require.NoError(t, err)

	var resp types.EBSConfigUpdateResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	assert.False(t, resp.Success)
	assert.Contains(t, resp.Error, "bad request:")
}

func TestEBSConfigQueueGroup_DetachedOpenFails(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	// No mounted volume, so the queue-group takes the detached branch and tries
	// openVolumeVB against the mock S3 host, which fails.
	cfg := setupTestConfig(t, natsURL)

	go func() { launchService(cfg) }()
	time.Sleep(500 * time.Millisecond)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	reqData, _ := json.Marshal(configReq(t, "vol-qg-detached", 3))
	msg, err := nc.Request("ebs.config", reqData, 5*time.Second)
	require.NoError(t, err)

	var resp types.EBSConfigUpdateResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	assert.False(t, resp.Success)
	assert.Contains(t, resp.Error, "open volume:")
}
