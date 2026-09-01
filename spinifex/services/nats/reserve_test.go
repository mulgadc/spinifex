package nats_test

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	svcnats "github.com/mulgadc/spinifex/spinifex/services/nats"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testReserveSize = 64 << 10

// fixedFree returns a probe that reports a constant amount of free space,
// which is the only way to drive the starved branch on a real filesystem.
func fixedFree(free uint64) func(string) (uint64, error) {
	return func(string) (uint64, error) { return free, nil }
}

func newTestReserve(t *testing.T, free uint64) *svcnats.StoreReserve {
	t.Helper()
	return &svcnats.StoreReserve{
		Dir:       filepath.Join(t.TempDir(), "nats"),
		Size:      testReserveSize,
		FreeBytes: fixedFree(free),
	}
}

// allocatedBytes reports the blocks actually charged to path, so a sparse
// file that reserves nothing is distinguishable from a real allocation.
func allocatedBytes(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	require.NoError(t, err)
	st, ok := fi.Sys().(*syscall.Stat_t)
	require.True(t, ok, "stat is not a syscall.Stat_t")
	return st.Blocks * 512
}

func TestStoreReserve_PathSitsBesideJetStreamDir(t *testing.T) {
	r := &svcnats.StoreReserve{Dir: "/var/lib/spinifex/nats"}
	assert.Equal(t, "/var/lib/spinifex/nats/"+svcnats.StoreReserveName, r.Path())
	assert.NotEqual(t, "jetstream", svcnats.StoreReserveName,
		"the reserve must never be mistaken for the jetstream directory")
}

func TestStoreReserve_ArmsWhenSpaceIsPlentiful(t *testing.T) {
	r := newTestReserve(t, 100*testReserveSize)

	released, err := r.Prepare()
	require.NoError(t, err)
	assert.False(t, released)

	fi, err := os.Stat(r.Path())
	require.NoError(t, err)
	assert.Equal(t, int64(testReserveSize), fi.Size())
}

func TestStoreReserve_ArmAllocatesRealBlocks(t *testing.T) {
	r := newTestReserve(t, 100*testReserveSize)

	_, err := r.Prepare()
	require.NoError(t, err)

	assert.GreaterOrEqual(t, allocatedBytes(t, r.Path()), int64(testReserveSize),
		"a sparse reserve would hold back no space at all")
}

func TestStoreReserve_ArmIsIdempotent(t *testing.T) {
	r := newTestReserve(t, 100*testReserveSize)

	_, err := r.Prepare()
	require.NoError(t, err)
	before, err := os.Stat(r.Path())
	require.NoError(t, err)

	released, err := r.Prepare()
	require.NoError(t, err)
	assert.False(t, released)

	after, err := os.Stat(r.Path())
	require.NoError(t, err)
	assert.Equal(t, before.Size(), after.Size())
}

func TestStoreReserve_ReleasesWhenStarved(t *testing.T) {
	r := newTestReserve(t, 100*testReserveSize)
	_, err := r.Prepare()
	require.NoError(t, err)
	require.FileExists(t, r.Path())

	// The filesystem has hit zero: the reserve is the only headroom left for
	// stream recovery, so it must be handed back.
	r.FreeBytes = fixedFree(0)
	released, err := r.Prepare()
	require.NoError(t, err)
	assert.True(t, released)
	assert.NoFileExists(t, r.Path())
}

func TestStoreReserve_ReleaseWhenAlreadyAbsentIsNotReported(t *testing.T) {
	r := newTestReserve(t, 0)

	released, err := r.Prepare()
	require.NoError(t, err)
	assert.False(t, released, "an absent reserve was not released by this call")
	assert.NoFileExists(t, r.Path())
}

func TestStoreReserve_HoldsSteadyOnMarginalFreeSpace(t *testing.T) {
	// Between one and two reserves free: arming would drop below the
	// threshold and release on the next start, so nothing should change.
	r := newTestReserve(t, testReserveSize+1)

	released, err := r.Prepare()
	require.NoError(t, err)
	assert.False(t, released)
	assert.NoFileExists(t, r.Path())
}

func TestStoreReserve_RearmsAfterSpaceIsFreed(t *testing.T) {
	r := newTestReserve(t, 0)
	_, err := r.Prepare()
	require.NoError(t, err)
	require.NoFileExists(t, r.Path())

	r.FreeBytes = fixedFree(100 * testReserveSize)
	released, err := r.Prepare()
	require.NoError(t, err)
	assert.False(t, released)
	assert.FileExists(t, r.Path())
}

func TestStoreReserve_DisabledWhenSizeIsZero(t *testing.T) {
	r := newTestReserve(t, 0)
	r.Size = 0

	released, err := r.Prepare()
	require.NoError(t, err)
	assert.False(t, released)
	assert.NoDirExists(t, r.Dir, "a disabled reserve must not touch the store dir")
}

func TestStoreReserve_DisabledWhenDirIsEmpty(t *testing.T) {
	r := &svcnats.StoreReserve{Size: testReserveSize}

	released, err := r.Prepare()
	require.NoError(t, err)
	assert.False(t, released)
}

func TestStoreReserve_CreatesMissingStoreDir(t *testing.T) {
	r := newTestReserve(t, 100*testReserveSize)
	require.NoDirExists(t, r.Dir)

	_, err := r.Prepare()
	require.NoError(t, err)
	assert.DirExists(t, r.Dir)
}

func TestStoreReserve_ReportsProbeFailure(t *testing.T) {
	sentinel := errors.New("probe failed")
	r := newTestReserve(t, 0)
	r.FreeBytes = func(string) (uint64, error) { return 0, sentinel }

	released, err := r.Prepare()
	assert.False(t, released)
	require.ErrorIs(t, err, sentinel)
}

func TestStoreReserve_ReportsUncreatableStoreDir(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "nats")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))

	r := &svcnats.StoreReserve{Dir: blocker, Size: testReserveSize, FreeBytes: fixedFree(0)}
	released, err := r.Prepare()
	assert.False(t, released)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "jetstream store dir")
}

// TestStoreReserve_DefaultProbeUsesStatfs exercises the production free-space
// path, which the injected probe otherwise hides.
func TestStoreReserve_DefaultProbeUsesStatfs(t *testing.T) {
	r := &svcnats.StoreReserve{Dir: filepath.Join(t.TempDir(), "nats"), Size: testReserveSize}

	released, err := r.Prepare()
	require.NoError(t, err)
	assert.False(t, released)
	assert.FileExists(t, r.Path(), "a temp dir should have room for a 64 KiB reserve")
}

func TestStoreReserve_ApplyArmsAndSurvivesFailure(t *testing.T) {
	r := newTestReserve(t, 100*testReserveSize)
	r.Apply()
	assert.FileExists(t, r.Path())

	// A probe failure must be swallowed so the server can still start.
	r.FreeBytes = func(string) (uint64, error) { return 0, errors.New("probe failed") }
	assert.NotPanics(t, r.Apply)
	assert.FileExists(t, r.Path())
}

func TestStoreReserve_ApplyReleasesWhenStarved(t *testing.T) {
	r := newTestReserve(t, 100*testReserveSize)
	r.Apply()
	require.FileExists(t, r.Path())

	r.FreeBytes = fixedFree(0)
	r.Apply()
	assert.NoFileExists(t, r.Path())
}

func TestStoreReserve_MaintainRearmsUntilStopped(t *testing.T) {
	r := newTestReserve(t, 0)
	r.Interval = time.Millisecond
	_, err := r.Prepare()
	require.NoError(t, err)
	require.NoFileExists(t, r.Path())

	stop := make(chan struct{})
	done := make(chan struct{})
	r.FreeBytes = fixedFree(100 * testReserveSize)
	go func() {
		r.Maintain(stop)
		close(done)
	}()

	require.Eventually(t, func() bool {
		_, err := os.Stat(r.Path())
		return err == nil
	}, 5*time.Second, 5*time.Millisecond, "Maintain should re-arm once space returns")

	close(stop)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Maintain did not return after stop was closed")
	}
}

func TestStoreReserve_MaintainStopsBeforeFirstTick(t *testing.T) {
	r := newTestReserve(t, 100*testReserveSize)
	stop := make(chan struct{})
	close(stop)

	done := make(chan struct{})
	go func() {
		r.Maintain(stop)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Maintain ignored an already-closed stop channel")
	}
	assert.NoFileExists(t, r.Path(), "a stopped maintainer must not arm")
}

func TestDefaultStoreReserveInterval_IsBounded(t *testing.T) {
	assert.Positive(t, svcnats.DefaultStoreReserveInterval)
	assert.LessOrEqual(t, svcnats.DefaultStoreReserveInterval, time.Hour,
		"a node that spent its headroom must re-arm well inside a working day")
}

func TestDefaultStoreReserveBytes_LeavesRecoveryHeadroom(t *testing.T) {
	assert.Positive(t, svcnats.DefaultStoreReserveBytes)
	assert.Less(t, svcnats.DefaultStoreReserveBytes, int64(1)<<30,
		"the reserve is held on every node and must stay small next to max_file_store")
}
