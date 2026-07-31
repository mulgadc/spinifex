package scratch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The sweep runs against a directory shared with other processes, so what it
// declines to touch matters as much as what it reclaims: only a directory that
// matches the prefix AND is far older than any live run may go.
func TestSweepAbandoned(t *testing.T) {
	tmpDir := t.TempDir()
	stale := time.Now().Add(-DefaultMaxAge - time.Hour)

	mkdir := func(name string, modTime time.Time) string {
		path := filepath.Join(tmpDir, name)
		require.NoError(t, os.MkdirAll(filepath.Join(path, "data"), 0o750))
		require.NoError(t, os.Chtimes(path, modTime, modTime))
		return path
	}

	abandoned := mkdir("fixture-1234567890", stale)
	concurrent := mkdir("fixture-0987654321", time.Now())
	otherPrefix := mkdir("some-other-tool-cache", stale)

	// A stale FILE sharing the prefix is not scratch and is not ours to delete.
	stalefile := filepath.Join(tmpDir, "fixture-notadir")
	require.NoError(t, os.WriteFile(stalefile, []byte("x"), 0o600))
	require.NoError(t, os.Chtimes(stalefile, stale, stale))

	SweepAbandoned(tmpDir, "fixture-", DefaultMaxAge)

	assert.NoDirExists(t, abandoned, "a stale scratch dir from a killed run must be reclaimed")
	assert.DirExists(t, concurrent, "a freshly-modified scratch dir may belong to a concurrent test binary")
	assert.DirExists(t, otherPrefix, "the sweep must not touch directories outside its own prefix")
	assert.FileExists(t, stalefile, "the sweep must only remove directories")
}

// A missing or unreadable directory is not a test failure: the sweep exists to
// reclaim disk, and failing to do so must never take the run down with it.
func TestSweepAbandonedToleratesUnreadableDir(t *testing.T) {
	assert.NotPanics(t, func() {
		SweepAbandoned(filepath.Join(t.TempDir(), "does-not-exist"), "fixture-", DefaultMaxAge)
	})
}
