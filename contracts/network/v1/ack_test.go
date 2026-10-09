package networkv1_test

import (
	"encoding/json"
	"testing"

	networkv1 "github.com/mulgadc/spinifex/contracts/network/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAckEnvelope_JSON pins both ack envelope shapes: bare success, and
// failure with its error populated. Error is omitempty, so the two must
// not be mistaken for the same shape.
func TestAckEnvelope_JSON(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		golden := `{"success":true}`
		env := networkv1.AckEnvelope{Success: true}

		marshaled, err := json.Marshal(env)
		require.NoError(t, err)
		assert.JSONEq(t, golden, string(marshaled))

		var decoded networkv1.AckEnvelope
		require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
		assert.Equal(t, env, decoded)
	})

	t.Run("error", func(t *testing.T) {
		golden := `{"success":false,"error":"boom"}`
		env := networkv1.AckEnvelope{Success: false, Error: "boom"}

		marshaled, err := json.Marshal(env)
		require.NoError(t, err)
		assert.JSONEq(t, golden, string(marshaled))

		var decoded networkv1.AckEnvelope
		require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
		assert.Equal(t, env, decoded)
	})
}
