package viperblock

// Tests for applyConfigUpdate, which ExpandVolume uses to persist a new size.

import (
	"encoding/json"
	"testing"

	"github.com/mulgadc/viperblock/viperblock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const gib = uint64(1024 * 1024 * 1024)

// configJSON marshals a VolumeConfig carrying the given size.
func configJSON(t *testing.T, volume string, sizeGiB uint64) json.RawMessage {
	t.Helper()
	vc := viperblock.VolumeConfig{}
	vc.VolumeMetadata.VolumeID = volume
	vc.VolumeMetadata.SizeGiB = sizeGiB
	raw, err := json.Marshal(vc)
	require.NoError(t, err)
	return raw
}

// --- applyConfigUpdate ---

func TestApplyConfigUpdate_GrowsVolumeSize(t *testing.T) {
	t.Parallel()
	vb := createTestVBWithState(t, "vol-apply-grow")

	require.NoError(t, applyConfigUpdate(t.Context(), vb, configJSON(t, "vol-apply-grow", 5)))

	assert.Equal(t, uint64(5), vb.VolumeConfig.VolumeMetadata.SizeGiB)
	assert.Equal(t, 5*gib, vb.VolumeSize)
}

func TestApplyConfigUpdate_ShrinkKeepsSize(t *testing.T) {
	t.Parallel()
	vb := createTestVBWithState(t, "vol-apply-shrink")
	vb.VolumeSize = 10 * gib

	require.NoError(t, applyConfigUpdate(t.Context(), vb, configJSON(t, "vol-apply-shrink", 2)))

	// Config metadata applied, but VolumeSize is grow-only.
	assert.Equal(t, uint64(2), vb.VolumeConfig.VolumeMetadata.SizeGiB)
	assert.Equal(t, 10*gib, vb.VolumeSize)
}

func TestApplyConfigUpdate_InvalidVolumeConfig(t *testing.T) {
	t.Parallel()
	vb := createTestVBWithState(t, "vol-apply-bad")

	err := applyConfigUpdate(t.Context(), vb, json.RawMessage(`"not-an-object"`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unmarshal VolumeConfig")
}
