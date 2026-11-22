package viperblock

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	ebsprovider "github.com/mulgadc/spinifex/spinifex/providers/ebs"
	testpredastore "github.com/mulgadc/spinifex/tests/fixtures/predastore"
	"github.com/mulgadc/viperblock/types"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/nats-io/nats.go/jetstream"
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

// lateAckKV runs after once an Update is applied, before its acknowledgement
// reaches the caller: the lease changes while the write is in flight.
type lateAckKV struct {
	jetstream.KeyValue

	after func()
}

func (k lateAckKV) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	rev, err := k.KeyValue.Update(ctx, key, value, revision)
	if k.after != nil {
		k.after()
	}
	return rev, err
}

// An epoch acknowledged after the lease was given up, or after its validity
// lapsed, authorises no launch.
func TestVolumeLease_LateEpochAcknowledgementTakesNoEpoch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		during func(*volumeLeases, *volumeLease)
	}{
		{"given up", func(_ *volumeLeases, lease *volumeLease) {
			lease.mu.Lock()
			lease.lost = true
			lease.mu.Unlock()
		}},
		{"lapsed", func(leases *volumeLeases, _ *volumeLease) {
			late := time.Now().Add(2 * volumeLeaseValidity)
			leases.now = func() time.Time { return late }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, natsURL := setupEmbeddedNATS(t)
			leases := newTestLeases(t, natsURL, "node-a")
			lease, err := leases.acquire(t.Context(), "vol-lateepoch-"+strings.ToLower(rand.Text()))
			require.NoError(t, err)
			lease.stop()
			var once sync.Once
			leases.kv = lateAckKV{KeyValue: leases.kv, after: func() { once.Do(func() { tc.during(leases, lease) }) }}
			_, err = lease.launchEpoch(t.Context())
			assert.ErrorIs(t, err, ErrLeaseStoreUnavailable)
		})
	}
}

func TestProviderHandlers_CreateVolume_FormatTwoNeedsALeaseStore(t *testing.T) {
	t.Parallel()
	cfg := &Config{}
	_, _, err := cfg.creationEpoch(t.Context(), "vol-noleasestore01")
	assert.ErrorIs(t, err, errNoVolumeLeaseStore)
}

// A format-2 VBState that records no committed head is an incomplete
// creation: the create retry recreates the volume rather than report it.
func TestProviderHandlers_CreateVolume_FormatTwoIncompleteCreationIsRetried(t *testing.T) {
	t.Parallel()
	cfg, create := formatTwoProvider(t)
	volumeName := "vol-fmt2retry-" + strings.ToLower(rand.Text())

	// A crashed attempt that wrote VBState, as a sequence reservation does,
	// before publishing any head.
	vbconfig := buildProviderVBConfig(cfg, volumeName, 1<<30, "", "")
	vbconfig.LeaseEpoch = 1
	crashed, err := viperblock.New(vbconfig, "s3", cfg.volumeS3Config(volumeName))
	require.NoError(t, err)
	require.NoError(t, crashed.Backend.InitCtx(t.Context()))
	require.NoError(t, crashed.InitMapDeltaCtx(t.Context(), false))
	require.NoError(t, crashed.SaveStateCtx(t.Context()))
	crashed.Detach()
	_, err = describeVolumeEngine(t.Context(), cfg, volumeName)
	require.ErrorIs(t, err, viperblock.ErrStateNotFound)

	require.Nil(t, create(marshalRequest(t, map[string]any{
		"schema_version": ebsprovider.SchemaVersion,
		"volume_id":      volumeName,
		"capacity_range": map[string]any{"required_bytes": int64(1) << 30},
	})).Error)
	state, err := readVolumeState(t.Context(), cfg, volumeName)
	require.NoError(t, err)
	assert.True(t, state.MapCommitted, "the retry completed the creation")
	_ = readOnlyEngine(t, cfg, volumeName)
}

// A create that paused after its first existence check, while another node
// finished the same volume, returns that volume and publishes nothing over it.
func TestProviderHandlers_CreateVolume_FormatTwoRechecksUnderTheLease(t *testing.T) {
	t.Parallel()
	cfg, create := formatTwoProvider(t)
	volumeName := "vol-fmt2recheck-" + strings.ToLower(rand.Text())
	var otherEpoch uint64
	cfg.afterCreateCheck = func(id string) {
		lease, epoch, err := cfg.creationEpoch(t.Context(), id)
		require.NoError(t, err)
		defer cfg.leases.release(t.Context(), lease)
		vbconfig := buildProviderVBConfig(cfg, id, 1<<30, "", "")
		vbconfig.LeaseEpoch = epoch
		other, err := viperblock.New(vbconfig, "s3", cfg.volumeS3Config(id))
		require.NoError(t, err)
		defer other.Detach()
		require.NoError(t, other.Backend.InitCtx(t.Context()))
		require.NoError(t, other.InitMapDeltaCtx(t.Context(), true))
		require.NoError(t, other.SaveStateCtx(t.Context()))
		otherEpoch = epoch
	}
	resp := create(marshalRequest(t, map[string]any{
		"schema_version": ebsprovider.SchemaVersion,
		"volume_id":      volumeName,
		"capacity_range": map[string]any{"required_bytes": int64(1) << 30},
	}))
	require.Nil(t, resp.Error)
	require.NotNil(t, resp.Volume)
	state, err := readVolumeState(t.Context(), cfg, volumeName)
	require.NoError(t, err)
	assert.Equal(t, otherEpoch, state.MapCreationEpoch, "the paused create published no map of its own")
	_ = readOnlyEngine(t, cfg, volumeName)
}

// A create that finds, under the lease, a volume another node finished with a
// different size refuses it rather than return that volume as its own.
func TestProviderHandlers_CreateVolume_RecheckRefusesADifferentCapacity(t *testing.T) {
	t.Parallel()
	cfg, create := formatTwoProvider(t)
	volumeName := "vol-fmt2recap-" + strings.ToLower(rand.Text())
	cfg.afterCreateCheck = func(id string) {
		lease, epoch, err := cfg.creationEpoch(t.Context(), id)
		require.NoError(t, err)
		defer cfg.leases.release(t.Context(), lease)
		vbconfig := buildProviderVBConfig(cfg, id, 2<<30, "", "")
		vbconfig.LeaseEpoch = epoch
		other, err := viperblock.New(vbconfig, "s3", cfg.volumeS3Config(id))
		require.NoError(t, err)
		defer other.Detach()
		require.NoError(t, other.Backend.InitCtx(t.Context()))
		require.NoError(t, other.InitMapDeltaCtx(t.Context(), true))
		require.NoError(t, other.SaveStateCtx(t.Context()))
	}
	resp := create(marshalRequest(t, map[string]any{
		"schema_version": ebsprovider.SchemaVersion,
		"volume_id":      volumeName,
		"capacity_range": map[string]any{"required_bytes": int64(1) << 30},
	}))
	require.NotNil(t, resp.Error)
	assert.Equal(t, ebsprovider.ErrorCodeAlreadyExists, resp.Error.Code)
	assert.Nil(t, resp.Volume)
}

// A converted volume whose VBState write was lost reads format 1 from
// VBState with the fence stub at its live key. The fallback seal must load it
// as format 2 and rewrite VBState, never write a format-1 map over it.
func TestSealVolumeVB_ConvertedVolumeWithAFormatOneVBStateSealsAsFormatTwo(t *testing.T) {
	t.Parallel()
	cfg, _ := formatTwoProvider(t)
	volumeName := "vol-convseal-" + strings.ToLower(rand.Text())
	seed := bytes.Repeat([]byte{0x5a}, 4096)

	// A format-1 volume, as a build without format 2 wrote it.
	vbconfig, s3cfg := volumeVBConfig(cfg, volumeName)
	vbconfig.VolumeSize = 1 << 30
	v1, err := viperblock.New(&vbconfig, "s3", s3cfg)
	require.NoError(t, err)
	require.NoError(t, v1.Backend.InitCtx(t.Context()))
	require.NoError(t, v1.SaveStateCtx(t.Context()))
	require.NoError(t, seedVolume(t.Context(), v1, seed))

	// Its format-2 chain and stubs are durable; the VBState write was not.
	conv := readOnlyEngine(t, cfg, volumeName)
	require.False(t, conv.IsMapDelta())
	num := conv.BlockToObjectWAL.WallNum.Load()
	v1Numbered, err := conv.Backend.ReadCtx(t.Context(), types.FileTypeBlockCheckpoint, num, 0, 0)
	require.NoError(t, err)
	conv.LeaseEpoch = 1
	require.NoError(t, conv.InitMapDeltaCtx(t.Context(), true))
	// A flip fences a new numbered key; the one VBState names keeps the v1 map.
	empty := []byte{}
	require.NoError(t, conv.Backend.WriteCtx(t.Context(), types.FileTypeBlockCheckpoint, num, &empty, &v1Numbered))
	conv.Detach()

	require.NoError(t, sealVolumeVB(t.Context(), cfg, volumeName))

	// VBState alone, as every open reads it before choosing a loader.
	stateOnly, s3cfg2 := volumeVBConfig(cfg, volumeName)
	st, err := viperblock.New(&stateOnly, "s3", s3cfg2)
	require.NoError(t, err)
	t.Cleanup(st.Detach)
	require.NoError(t, st.Backend.InitCtx(t.Context()))
	require.NoError(t, st.LoadStateCtx(t.Context()))
	assert.True(t, st.IsMapDelta(), "the seal rewrote VBState as format 2")

	after := readOnlyEngine(t, cfg, volumeName)
	live, err := after.Backend.ReadCtx(t.Context(), types.FileTypeBlockCheckpointLive, 0, 0, 0)
	require.NoError(t, err)
	assert.True(t, viperblock.IsMapFenceStub(live), "the seal did not write a format-1 map over the stub")
	got, err := after.ReadAt(0, 4096)
	require.NoError(t, err)
	assert.Equal(t, seed, got)
}

// A create releases the new volume's lease before it replies, so a caller that
// publishes or snapshots the volume as soon as it exists never finds it held.
func TestProviderHandlers_CreateVolume_ReleasesTheLeaseBeforeReplying(t *testing.T) {
	t.Parallel()
	cfg, create := formatTwoProvider(t)
	volumeName := "vol-leasereply-" + strings.ToLower(rand.Text())
	var replied bool
	cfg.onCreateReply = func(id string) {
		replied = true
		_, err := cfg.leases.kv.Get(t.Context(), id)
		assert.Error(t, err, "the lease is still held when the create replies")
	}
	resp := create(marshalRequest(t, map[string]any{
		"schema_version": ebsprovider.SchemaVersion,
		"volume_id":      volumeName,
		"capacity_range": map[string]any{"required_bytes": int64(1) << 30},
	}))
	require.Nil(t, resp.Error)
	assert.True(t, replied, "the create replied without passing through its lease release")
}
