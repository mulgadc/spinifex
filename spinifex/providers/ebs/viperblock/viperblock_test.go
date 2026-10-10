package viperblock

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	vbtypes "github.com/mulgadc/viperblock/types"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigLoadStateRetryPolicy(t *testing.T) {
	attempts, delay := (&Config{}).loadStateRetryPolicy()
	assert.Equal(t, defaultLoadStateRetryAttempts, attempts)
	assert.Equal(t, defaultLoadStateRetryBaseDelay, delay)

	cfg := &Config{loadStateRetryAttempts: 2, loadStateRetryBaseDelay: time.Nanosecond}
	attempts, delay = cfg.loadStateRetryPolicy()
	assert.Equal(t, 2, attempts)
	assert.Equal(t, time.Nanosecond, delay)
}

// TestReadVolumeState_BadRequestFailsFast covers all three retry layers with
// the production budgets: one HTTP 400 must produce one prompt request.
func TestReadVolumeState_BadRequestFailsFast(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)

	cfg := &Config{
		S3Host:    srv.URL,
		Bucket:    "bucket",
		Region:    "us-east-1",
		AccessKey: "access-key",
		SecretKey: "secret-key",
		BaseDir:   t.TempDir(),
	}

	start := time.Now()
	_, err := readVolumeState(context.Background(), cfg, "vol-rejected")
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.ErrorIs(t, err, vbtypes.ErrBackendNonRetryable)
	assert.Equal(t, int32(1), requests.Load())
	assert.Less(t, elapsed, time.Second)
}

// TestRetryLoadState pins the retry policy: transient errors retry with backoff,
// ErrStateNotFound fails fast, and the retry budget is honoured.
func TestRetryLoadState(t *testing.T) {
	t.Run("success_first_attempt_no_sleep", func(t *testing.T) {
		calls := 0
		sleeps := 0
		err := retryLoadState("vol-x", 5, 10*time.Millisecond,
			func(time.Duration) { sleeps++ },
			func() error { calls++; return nil })
		require.NoError(t, err)
		assert.Equal(t, 1, calls)
		assert.Equal(t, 0, sleeps)
	})

	t.Run("transient_recovers_within_budget", func(t *testing.T) {
		calls := 0
		sleeps := 0
		err := retryLoadState("vol-x", 5, 10*time.Millisecond,
			func(time.Duration) { sleeps++ },
			func() error {
				calls++
				if calls < 3 {
					return fmt.Errorf("wrap: %w", viperblock.ErrStateBackendUnavailable)
				}
				return nil
			})
		require.NoError(t, err)
		assert.Equal(t, 3, calls)
		assert.Equal(t, 2, sleeps)
	})

	t.Run("not_found_fails_fast", func(t *testing.T) {
		calls := 0
		sleeps := 0
		err := retryLoadState("vol-x", 5, 10*time.Millisecond,
			func(time.Duration) { sleeps++ },
			func() error { calls++; return viperblock.ErrStateNotFound })
		require.Error(t, err)
		assert.ErrorIs(t, err, viperblock.ErrStateNotFound)
		assert.Equal(t, 1, calls, "ErrStateNotFound must not be retried")
		assert.Equal(t, 0, sleeps)
	})

	t.Run("unclassified_fails_fast", func(t *testing.T) {
		calls := 0
		sentinel := errors.New("permission denied")
		err := retryLoadState("vol-x", 5, 10*time.Millisecond,
			func(time.Duration) {},
			func() error { calls++; return sentinel })
		require.Error(t, err)
		assert.ErrorIs(t, err, sentinel)
		assert.Equal(t, 1, calls)
	})

	t.Run("exhausts_budget_on_persistent_transient", func(t *testing.T) {
		calls := 0
		sleeps := 0
		var lastDelay time.Duration
		err := retryLoadState("vol-x", 4, 100*time.Millisecond,
			func(d time.Duration) { sleeps++; lastDelay = d },
			func() error { calls++; return viperblock.ErrStateBackendUnavailable })
		require.Error(t, err)
		assert.ErrorIs(t, err, viperblock.ErrStateBackendUnavailable)
		assert.Equal(t, 4, calls)
		assert.Equal(t, 3, sleeps, "sleep happens between attempts, not after the final one")
		// Backoff multiplier is 1.5: 100ms, 150ms, 225ms.
		assert.Equal(t, 225*time.Millisecond, lastDelay)
	})
}

// TestValidVolumeName covers the character-level gate every handler that
// turns a wire volume name into a filesystem path must pass first.
func TestValidVolumeName(t *testing.T) {
	valid := []string{"vol-0123456789abcdef0", "vol-0123456789abcdef0-efi", "a"}
	for _, name := range valid {
		assert.True(t, validVolumeName(name), "expected %q to be valid", name)
	}

	invalid := []string{"", ".", "..", "../..", "a/b", "/etc/passwd", "vol/../.."}
	for _, name := range invalid {
		assert.False(t, validVolumeName(name), "expected %q to be rejected", name)
	}
}

// TestLocalVolumeDir is the regression test for the ebs.delete data-destruction
// hazard: an unvalidated volume name reaching os.RemoveAll could wipe BaseDir
// itself (empty name) or escape it entirely ("../.."). Every case here must be
// rejected before a caller ever gets a path back to remove.
func TestLocalVolumeDir(t *testing.T) {
	baseDir := t.TempDir()

	t.Run("empty_volume_rejected", func(t *testing.T) {
		_, err := localVolumeDir(baseDir, "")
		require.Error(t, err, "an empty volume must never resolve to BaseDir itself")
	})

	t.Run("dot_rejected", func(t *testing.T) {
		_, err := localVolumeDir(baseDir, ".")
		require.Error(t, err)
	})

	t.Run("parent_traversal_rejected", func(t *testing.T) {
		for _, name := range []string{"..", "../..", "../../etc"} {
			_, err := localVolumeDir(baseDir, name)
			require.Error(t, err, "volume %q must not escape BaseDir", name)
		}
	})

	t.Run("embedded_separator_rejected", func(t *testing.T) {
		for _, name := range []string{"a/b", "a/../../b", "/abs/path"} {
			_, err := localVolumeDir(baseDir, name)
			require.Error(t, err, "volume %q must not contain a path separator", name)
		}
	})

	t.Run("empty_base_dir_rejected", func(t *testing.T) {
		_, err := localVolumeDir("", "vol-abc123")
		require.Error(t, err)
	})

	t.Run("valid_name_resolves_under_base_dir", func(t *testing.T) {
		dir, err := localVolumeDir(baseDir, "vol-abc123")
		require.NoError(t, err)
		absBase, err := filepath.Abs(baseDir)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(absBase, "vol-abc123"), dir)
	})
}
