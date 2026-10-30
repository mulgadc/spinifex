package viperblock

//test:in-package — the dirty marker, the lease store and acquireVolumeLease are unexported.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLiveness answers LastSeen from fixed values and counts the questions.
type fakeLiveness struct {
	seen  time.Time
	found bool
	err   error
	asked int
}

func (f *fakeLiveness) LastSeen(context.Context, string) (time.Time, bool, error) {
	f.asked++
	return f.seen, f.found, f.err
}

// takeoverFixture leaves a marker naming node-a and returns node-b's config,
// the config that is about to open the volume.
func takeoverFixture(t *testing.T, volume string, liveness nodeLiveness) *Config {
	t.Helper()
	_, natsURL := setupEmbeddedNATS(t)

	owner := newTestDirty(t, natsURL, "node-a")
	require.NoError(t, owner.mark(t.Context(), volume, 1, "seal volume: predastore unreachable"))

	cfg := &Config{
		leases: newTestLeases(t, natsURL, "node-b"),
		dirty:  newTestDirty(t, natsURL, "node-b"),
	}
	if liveness != nil {
		cfg.liveness = liveness
	}
	return cfg
}

func requireMarkerStillNames(t *testing.T, cfg *Config, volume, owner string) {
	t.Helper()
	record, ok := cfg.dirty.holder(t.Context(), volume)
	require.True(t, ok)
	assert.Equal(t, owner, record.Owner, "a refused takeover must leave the marker with the node that holds the writes")
}

// A node still heartbeating holds its unsealed writes in memory, and opening
// the volume from the backend checkpoint would silently discard them.
func TestVolumeTakeover_RefusedWhileTheMarkerOwnerIsLive(t *testing.T) {
	const volume = "vol-takeoverlive"
	cfg := takeoverFixture(t, volume, &fakeLiveness{seen: time.Now().Add(-5 * time.Second), found: true})

	lease, err := cfg.acquireVolumeLease(t.Context(), volume)

	require.ErrorIs(t, err, errVolumeLeaseHeld, "recovery classifies this as held elsewhere and retries")
	assert.Nil(t, lease)
	requireMarkerStillNames(t, cfg, volume, "node-a")
}

func TestVolumeTakeover_AllowedOnceTheMarkerOwnerIsStale(t *testing.T) {
	const volume = "vol-takeoverstale"
	cfg := takeoverFixture(t, volume, &fakeLiveness{seen: time.Now().Add(-2 * nodeLivenessWindow), found: true})

	_, err := cfg.acquireVolumeLease(t.Context(), volume)
	require.NoError(t, err)

	record, ok := cfg.dirty.holder(t.Context(), volume)
	require.True(t, ok)
	assert.Equal(t, "node-b", record.Owner)
	assert.Contains(t, record.Reason, "took over from node-a")
}

func TestVolumeTakeover_AllowedWhenTheMarkerOwnerHasNoHeartbeat(t *testing.T) {
	const volume = "vol-takeoverunseen"
	cfg := takeoverFixture(t, volume, &fakeLiveness{found: false})

	_, err := cfg.acquireVolumeLease(t.Context(), volume)
	require.NoError(t, err, "a node with no heartbeat record is gone")
}

func TestVolumeTakeover_NoMarkerNeverAsksAboutLiveness(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	liveness := &fakeLiveness{err: errors.New("must not be asked")}
	cfg := &Config{
		leases:   newTestLeases(t, natsURL, "node-b"),
		dirty:    newTestDirty(t, natsURL, "node-b"),
		liveness: liveness,
	}

	_, err := cfg.acquireVolumeLease(t.Context(), "vol-takeovernomarker")
	require.NoError(t, err)
	assert.Zero(t, liveness.asked)
}

// An unreadable heartbeat is not evidence that the owner is gone.
func TestVolumeTakeover_RefusedWhenLivenessCannotBeRead(t *testing.T) {
	const volume = "vol-takeovererror"
	cfg := takeoverFixture(t, volume, &fakeLiveness{err: errors.New("kv unavailable")})

	_, err := cfg.acquireVolumeLease(t.Context(), volume)

	require.ErrorIs(t, err, errVolumeLeaseHeld)
	requireMarkerStillNames(t, cfg, volume, "node-a")
}

func TestVolumeTakeover_RefusedWhenNoLivenessStoreIsWired(t *testing.T) {
	const volume = "vol-takeovernostore"
	cfg := takeoverFixture(t, volume, nil)

	_, err := cfg.acquireVolumeLease(t.Context(), volume)

	require.ErrorIs(t, err, errVolumeLeaseHeld)
	requireMarkerStillNames(t, cfg, volume, "node-a")
}

// Refusing without a store is only safe if production always has one, or every
// takeover in the cluster would be refused.
func TestRegisterProviderSubjects_WiresALivenessStore(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	startProviderSubjects(t, cfg, natsURL)

	assert.NotNil(t, cfg.liveness, "a production Config must get a liveness store")
}

// The daemon writes heartbeats under clusterv1.HeartbeatKey with a JSON record
// carrying more than the timestamp. The reader must date one it did not shape.
func TestHeartbeatLiveness_ReadsWhatTheDaemonWrites(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	live := newHeartbeatLiveness(nc)
	_, _, err = live.LastSeen(t.Context(), "node-a")
	require.Error(t, err, "a missing bucket is unknown, never a reason to call a node gone")

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: clusterv1.ClusterStateBucket})
	require.NoError(t, err)

	_, found, err := live.LastSeen(t.Context(), "node-a")
	require.NoError(t, err)
	assert.False(t, found, "no heartbeat record means the node has not been seen")

	beat, err := json.Marshal(map[string]any{
		"node": "node-a", "epoch": 3, "vm_count": 4, "allocated_vcpu": 8,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), clusterv1.HeartbeatKey("node-a"), beat)
	require.NoError(t, err)

	seen, found, err := live.LastSeen(t.Context(), "node-a")
	require.NoError(t, err)
	require.True(t, found)
	assert.WithinDuration(t, time.Now(), seen, nodeLivenessWindow)

	_, err = kv.Put(t.Context(), clusterv1.HeartbeatKey("node-b"), []byte("{"))
	require.NoError(t, err)
	_, found, err = live.LastSeen(t.Context(), "node-b")
	require.NoError(t, err)
	assert.False(t, found, "a record that cannot be dated vouches for nothing")
}
