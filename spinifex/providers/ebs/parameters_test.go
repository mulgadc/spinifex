package ebsprovider

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVolumeParameters(t *testing.T) {
	t.Run("round trip", func(t *testing.T) {
		raw := NewVolumeParameters("io1")
		assert.JSONEq(t, `{"volume_type":"io1"}`, string(raw))
		params, err := ParseVolumeParameters(raw)
		require.NoError(t, err)
		assert.Equal(t, "io1", params.VolumeType)
	})

	t.Run("absent parameters have no volume type", func(t *testing.T) {
		params, err := ParseVolumeParameters(nil)
		require.NoError(t, err)
		assert.Empty(t, params.VolumeType)
	})

	t.Run("malformed parameters are invalid", func(t *testing.T) {
		_, err := ParseVolumeParameters(json.RawMessage(`{"volume_type":`))
		require.ErrorIs(t, err, ErrInvalidArgument)
	})
}
