package viperblock

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mulgadc/viperblock/viperblock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func absentVolumeNode(t *testing.T, natsURL, node string, liveness nodeLiveness) *Config {
	t.Helper()
	return &Config{
		leases:   newTestLeases(t, natsURL, node),
		dirty:    newTestDirty(t, natsURL, node),
		liveness: liveness,
		constructVB: func(context.Context, string) (*viperblock.VB, int, error) {
			return nil, 0, fmt.Errorf("load state: %w", viperblock.ErrStateNotFound)
		},
	}
}

// Opening a volume that does not exist must not leave a marker behind, or a
// later open on another node is refused for a volume that is not there.
func TestBuildVB_AbsentVolumeLeavesNoMarkerAndStaysNotFound(t *testing.T) {
	const volume = "vol-neverexisted"
	_, natsURL := setupEmbeddedNATS(t)
	live := &fakeLiveness{seen: time.Now().Add(-2 * time.Second), found: true}
	first := absentVolumeNode(t, natsURL, "node-1", live)
	second := absentVolumeNode(t, natsURL, "node-2", live)

	_, _, _, err := first.buildVB(t.Context(), volume)
	require.ErrorIs(t, err, viperblock.ErrStateNotFound)
	_, found := first.dirty.holder(t.Context(), volume)
	assert.False(t, found, "an open that found no volume must not leave a marker")

	_, _, _, err = second.buildVB(t.Context(), volume)
	require.ErrorIs(t, err, viperblock.ErrStateNotFound, "a live first node must not turn not-found into held-elsewhere")
	assert.NotErrorIs(t, err, errVolumeLeaseHeld)
	_, found = second.dirty.holder(t.Context(), volume)
	assert.False(t, found)
}

// Clearing is for the marker this open wrote. Another volume's marker, which
// names the unsealed writes of a volume that does exist, stays.
func TestBuildVB_AbsentVolumeDoesNotTouchAnotherVolumesMarker(t *testing.T) {
	const held, absent = "vol-realunsealed", "vol-neverexisted2"
	_, natsURL := setupEmbeddedNATS(t)
	cfg := absentVolumeNode(t, natsURL, "node-1", &fakeLiveness{})
	require.NoError(t, cfg.dirty.mark(t.Context(), held, 1, "seal volume: predastore unreachable"))

	_, _, _, err := cfg.buildVB(t.Context(), absent)
	require.ErrorIs(t, err, viperblock.ErrStateNotFound)

	record, found := cfg.dirty.holder(t.Context(), held)
	require.True(t, found)
	assert.Equal(t, "node-1", record.Owner)
	assert.Equal(t, "seal volume: predastore unreachable", record.Reason)
}
