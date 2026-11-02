//go:build e2e

package multinode

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEnvDurationOr(t *testing.T) {
	const key = "SPINIFEX_MULTINODE_TEST_DURATION"

	t.Run("unset returns default", func(t *testing.T) {
		t.Setenv(key, "")
		require.Equal(t, 90*time.Second, envDurationOr(key, 90*time.Second))
	})

	t.Run("set overrides default", func(t *testing.T) {
		t.Setenv(key, "45s")
		require.Equal(t, 45*time.Second, envDurationOr(key, 90*time.Second))
	})

	for _, bad := range []string{"90", "soon", "0s", "-3s"} {
		t.Run("panics on "+bad, func(t *testing.T) {
			t.Setenv(key, bad)
			require.PanicsWithValue(t, key+`="`+bad+`" is not a positive duration`, func() {
				envDurationOr(key, 90*time.Second)
			})
		})
	}
}
