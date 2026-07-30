// Package scratch reclaims test scratch directories stranded in the system
// temp directory by earlier runs.
//
// Fixtures whose daemon outlives any individual test cannot use t.TempDir():
// the first test to finish would delete files the shared daemon still has
// open. They use os.MkdirTemp instead, which nothing then removes when the
// process dies without running its cleanups — a test timeout panic or an OOM
// kill. /tmp is a tmpfs on the dev boxes, so a stranded directory is charged
// to RAM and swap rather than disk, and swapped tmpfs pages have no backing
// store to be dropped to: they are never reclaimed until the files go.
package scratch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultMaxAge is how old a stranded directory must be before it is
// reclaimed. A full run takes minutes, so a directory this old cannot belong
// to a live test binary.
const DefaultMaxAge = 6 * time.Hour

// SweepAbandoned removes directories under tmpDir that match prefix and are
// older than maxAge.
//
// Deleting shared scratch deserves caution, so a candidate must match on both
// the prefix and the age: a concurrently running test binary's directory is
// minutes old at most and can never qualify. Failures are reported and
// otherwise ignored — not reclaiming disk must never fail the run.
func SweepAbandoned(tmpDir, prefix string, maxAge time.Duration) {
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scratch sweep: read %s: %v\n", tmpDir, err)
		return
	}

	cutoff := time.Now().Add(-maxAge)
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}

		path := filepath.Join(tmpDir, entry.Name())
		if err := os.RemoveAll(path); err != nil {
			fmt.Fprintf(os.Stderr, "scratch sweep: remove %s: %v\n", path, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "scratch sweep: reclaimed abandoned scratch dir %s\n", path)
	}
}
