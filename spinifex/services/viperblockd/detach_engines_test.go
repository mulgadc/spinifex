package viperblockd

//test:in-package — covers the unexported shutdownVolumes and reuses the
// in-package doubles captureLogs/createTestVBWithState/volumeOpenCount that
// the rest of this package's tests are built on.

// Tests proving a teardown that abandons an engine reports the volume
// released, pinned on the same "viperblock volume closed" record a production
// alarm would watch alongside the open.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// volumeCloseCount is volumeOpenCount's counterpart. An open with no matching
// close leaves the volume reading as permanently held, so the two are only
// meaningful together.
func volumeCloseCount(logs, volumeID string) int {
	count := 0
	for line := range strings.SplitSeq(logs, "\n") {
		if !strings.Contains(line, `msg="viperblock volume closed"`) {
			continue
		}
		if slices.Contains(strings.Fields(line), "volume="+volumeID) {
			count++
		}
	}
	return count
}

// TestShutdownVolumesReleasesTheEngine covers the daemon going down with
// volumes still mounted. The engine is abandoned rather than closed, so
// without a release here every volume this daemon ever mounted stays counted
// as held for as long as the metric series lives.
func TestShutdownVolumesReleasesTheEngine(t *testing.T) {
	logs := captureLogs(t)
	volumeID := "vol-shutdownrelease"
	vb := createTestVBWithState(t, volumeID)

	require.Equal(t, 1, volumeOpenCount(logs.String(), volumeID),
		"the engine must have recorded an open, or the close below has nothing to match")

	shutdownVolumes([]MountedVolume{{Name: volumeID, VB: vb, PID: os.Getpid()}},
		func(MountedVolume) bool { return true })

	assert.Equal(t, 1, volumeCloseCount(logs.String(), volumeID),
		"shutdown must report the volume released")
}

// TestNoTeardownStopsGoroutinesWithoutDetaching pins the fix. Stopping the
// background goroutines by hand looks like a teardown but reports nothing, so
// the engine count only ever counts up. Detach is the one way to end an engine
// that is not Close.
//
// The single exception is the state-tracking VB the mount path keeps alive
// while the nbdkit plugin owns the data path: it stops its uploader precisely
// because it is NOT finished with the volume.
func TestNoTeardownStopsGoroutinesWithoutDetaching(t *testing.T) {
	const allowedFile = "provider_handlers.go"
	const allowedContext = "This daemon-side VB tracks state only"

	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		require.NoError(t, err)
		checked++

		lines := strings.Split(string(src), "\n")
		for i, line := range lines {
			if !strings.Contains(line, "StopChunkUploader()") && !strings.Contains(line, "StopWALSyncer()") {
				continue
			}
			// Look back a few lines for the comment that documents the one
			// engine which stops a goroutine without being finished.
			window := strings.Join(lines[max(0, i-4):i], "\n")
			if name == allowedFile && strings.Contains(window, allowedContext) {
				continue
			}
			t.Errorf("%s:%d stops a background goroutine outside Detach: %q\n"+
				"an engine torn down this way never reports its release", name, i+1, strings.TrimSpace(line))
		}
	}
	require.Positive(t, checked, "no sources scanned — this would pass vacuously")
}
