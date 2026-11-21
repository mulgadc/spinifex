package viperblock

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"

	ebsprovider "github.com/mulgadc/spinifex/spinifex/providers/ebs"
	testpredastore "github.com/mulgadc/spinifex/tests/fixtures/predastore"
	"github.com/mulgadc/viperblock/types"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every launch takes an epoch above every earlier launch's: a shared open on
// the same node, and a later holder on another node, included.
func TestVolumeLease_EveryLaunchTakesAHigherEpoch(t *testing.T) {
	t.Parallel()
	_, natsURL := setupEmbeddedNATS(t)
	nodeA := newTestLeases(t, natsURL, "node-a")
	nodeB := newTestLeases(t, natsURL, "node-b")
	const volumeName = "vol-launchepoch01"

	first, err := nodeA.acquire(t.Context(), volumeName)
	require.NoError(t, err)
	e1, err := first.launchEpoch(t.Context())
	require.NoError(t, err)
	shared, err := nodeA.acquire(t.Context(), volumeName)
	require.NoError(t, err)
	require.Same(t, first, shared, "a second open on this node shares the lease")
	e2, err := shared.launchEpoch(t.Context())
	require.NoError(t, err)
	assert.Greater(t, e2, e1, "a shared open must not reuse the first launch's epoch")
	assert.True(t, first.renew(t.Context()), "a launch epoch must not break the next renewal")

	nodeA.release(t.Context(), shared)
	nodeA.release(t.Context(), first)
	next, err := nodeB.acquire(t.Context(), volumeName)
	require.NoError(t, err)
	e3, err := next.launchEpoch(t.Context())
	require.NoError(t, err)
	assert.Greater(t, e3, e2)
}

func formatTwoProvider(t *testing.T) (*Config, func(body []byte) ebsprovider.CreateVolumeResponse) {
	t.Helper()
	fixture := testpredastore.Start(t)
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	cfg.S3Host = "https://" + fixture.Host
	cfg.Bucket = testpredastore.DefaultBucket
	cfg.Region = fixture.Region
	cfg.AccessKey = fixture.AccessKey
	cfg.SecretKey = fixture.SecretKey
	cfg.MapFormatDefault = viperblock.MapFormatDelta
	nc := startProviderSubjects(t, cfg, natsURL)
	return cfg, func(body []byte) ebsprovider.CreateVolumeResponse {
		msg := requestProvider(t, nc, ebsprovider.CreateVolumeSubject, body)
		var resp ebsprovider.CreateVolumeResponse
		require.NoError(t, json.Unmarshal(msg.Data, &resp))
		return resp
	}
}

// readOnlyEngine loads a volume without a lease epoch, as a reader may.
func readOnlyEngine(t *testing.T, cfg *Config, volumeName string) *viperblock.VB {
	t.Helper()
	vbconfig, s3cfg := volumeVBConfig(cfg, volumeName)
	vb, err := viperblock.New(&vbconfig, "s3", s3cfg)
	require.NoError(t, err)
	t.Cleanup(vb.Detach)
	require.NoError(t, vb.Backend.InitCtx(t.Context()))
	require.NoError(t, vb.LoadStateCtx(t.Context()))
	require.NoError(t, vb.LoadLiveCheckpointCtx(t.Context()))
	return vb
}

func TestProviderHandlers_CreateVolume_FormatTwoFencesV1AndWritesStateLast(t *testing.T) {
	t.Parallel()
	cfg, create := formatTwoProvider(t)
	volumeName := "vol-fmt2create-" + strings.ToLower(rand.Text())
	resp := create(marshalRequest(t, map[string]any{
		"schema_version": ebsprovider.SchemaVersion,
		"volume_id":      volumeName,
		"capacity_range": map[string]any{"required_bytes": int64(1) << 30},
	}))
	require.Nil(t, resp.Error)

	vb := readOnlyEngine(t, cfg, volumeName)
	assert.True(t, vb.IsMapDelta(), "VBState records format 2")
	live, err := vb.Backend.ReadCtx(t.Context(), types.FileTypeBlockCheckpointLive, 0, 0, 0)
	require.NoError(t, err)
	assert.True(t, viperblock.IsMapFenceStub(live), "the live key is fenced against v1 readers")
	numbered, err := vb.Backend.ReadCtx(t.Context(), types.FileTypeBlockCheckpoint, vb.BlockToObjectWAL.WallNum.Load(), 0, 0)
	require.NoError(t, err)
	assert.True(t, viperblock.IsMapFenceStub(numbered), "so is the numbered key the fallback seal reads")
}

func TestProviderHandlers_CreateVolume_FormatTwoSeedReadsBackAndSeals(t *testing.T) {
	t.Parallel()
	cfg, create := formatTwoProvider(t)
	volumeName := "vol-fmt2seed-" + strings.ToLower(rand.Text())
	seed := bytes.Repeat([]byte("seeded-format-two!"), 512)
	resp := create(marshalRequest(t, map[string]any{
		"schema_version": ebsprovider.SchemaVersion,
		"volume_id":      volumeName,
		"capacity_range": map[string]any{"required_bytes": int64(1) << 30},
		"seed_data":      seed,
	}))
	require.Nil(t, resp.Error)

	vb := readOnlyEngine(t, cfg, volumeName)
	got, err := vb.ReadAt(0, uint64(len(seed)))
	require.NoError(t, err)
	assert.Equal(t, seed, got)
	vb.Detach()

	// The daemon's own opener takes a newer epoch and claims before it seals.
	require.NoError(t, sealVolumeVB(t.Context(), cfg, volumeName))
	again := readOnlyEngine(t, cfg, volumeName)
	got, err = again.ReadAt(0, uint64(len(seed)))
	require.NoError(t, err)
	assert.Equal(t, seed, got, "the seal's claim and commit keep the seed")
}

func TestVolumeLease_LostLeaseTakesNoEpoch(t *testing.T) {
	t.Parallel()
	_, natsURL := setupEmbeddedNATS(t)
	leases := newTestLeases(t, natsURL, "node-a")
	lease, err := leases.acquire(t.Context(), "vol-lostepoch0001")
	require.NoError(t, err)
	lease.mu.Lock()
	lease.lost = true
	lease.mu.Unlock()
	_, err = lease.launchEpoch(t.Context())
	assert.ErrorIs(t, err, ErrLeaseStoreUnavailable)
}

func TestProviderHandlers_CreateVolume_FormatTwoNeedsALeaseStore(t *testing.T) {
	t.Parallel()
	cfg := &Config{MapFormatDefault: viperblock.MapFormatDelta}
	_, _, err := cfg.creationEpoch(t.Context(), "vol-noleasestore01")
	assert.ErrorIs(t, err, errNoVolumeLeaseStore)
}

// A format-2 VBState with no committed head is an incomplete creation: the
// create retry recreates the volume rather than report it created.
func TestProviderHandlers_CreateVolume_FormatTwoIncompleteCreationIsRetried(t *testing.T) {
	t.Parallel()
	cfg, create := formatTwoProvider(t)
	volumeName := "vol-fmt2retry-" + strings.ToLower(rand.Text())
	body := marshalRequest(t, map[string]any{
		"schema_version": ebsprovider.SchemaVersion,
		"volume_id":      volumeName,
		"capacity_range": map[string]any{"required_bytes": int64(1) << 30},
	})
	require.Nil(t, create(body).Error)

	// Drop every head, as a crash between the state write and the first
	// head would have left the volume.
	vb := readOnlyEngine(t, cfg, volumeName)
	keys, err := vb.Backend.ListObjectsCtx(t.Context(), volumeName+"/map/")
	require.NoError(t, err)
	for _, k := range keys {
		if strings.Contains(k, "/map/head.") {
			require.NoError(t, vb.Backend.DeleteKeyCtx(t.Context(), strings.TrimPrefix(k, volumeName+"/")))
		}
	}
	vb.Detach()
	_, err = describeVolumeEngine(t.Context(), cfg, volumeName)
	require.ErrorIs(t, err, viperblock.ErrStateNotFound)

	require.Nil(t, create(body).Error)
	committed, err := volumeMapCommitted(t.Context(), cfg, volumeName)
	require.NoError(t, err)
	assert.True(t, committed, "the retry completed the creation")
}
