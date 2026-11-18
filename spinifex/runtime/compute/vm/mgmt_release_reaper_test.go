package vm

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pendingRelease leaves i-1's proven-stopped generation 1 marked pending after
// the address record refused its release.
func pendingRelease(t *testing.T, r *mgmtRig) {
	t.Helper()
	r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 1, State: MgmtLaunchRunning, PID: 4001, StartTime: 1})
	r.addrs.held[mgmtTestIP] = "i-1"
	r.addrs.failRel = errors.New("record unavailable")
	r.m.releaseMgmt(mgmtVM("i-1"), 1)
	r.addrs.failRel = nil
	require.True(t, r.record(t, "i-1").ReleasePending)
	require.Equal(t, "i-1", r.addrs.held[mgmtTestIP], "an unacknowledged release keeps the address")
}

func TestMgmtReleaseReaper_RetriesAFailedRelease(t *testing.T) {
	r := newMgmtRig(t)
	pendingRelease(t, r)

	n, err := r.m.NewMgmtReleaseReaper().Sweep(t.Context())

	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Empty(t, r.addrs.held)
	rec := r.record(t, "i-1")
	assert.False(t, rec.ReleasePending)
	assert.Equal(t, MgmtLaunchExited, rec.State)
}

// The pending mark is on disk, so a daemon that restarts finishes the release
// with no VM object for the instance.
func TestMgmtReleaseReaper_RetriesAfterRestart(t *testing.T) {
	r := newMgmtRig(t)
	pendingRelease(t, r)
	restarted := NewManagerWithDeps(Deps{NodeID: mgmtTestNode,
		MgmtLaunch: MgmtLaunchDeps{Dir: r.dir, Addresses: r.addrs}})

	n, err := restarted.NewMgmtReleaseReaper().Sweep(t.Context())

	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Empty(t, r.addrs.held)
}

// A launch that began after the failed release owns the address again, so the
// retry for the earlier generation must not release it.
func TestMgmtReleaseReaper_NewerLaunchKeepsTheAddress(t *testing.T) {
	r := newMgmtRig(t)
	pendingRelease(t, r)
	calls := 0
	require.NoError(t, r.m.startGuestQEMU(mgmtVM("i-1"), r.starter(t, 4002, 2, &calls), false))

	n, err := r.m.NewMgmtReleaseReaper().Sweep(t.Context())

	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Equal(t, "i-1", r.addrs.held[mgmtTestIP])
	rec := r.record(t, "i-1")
	assert.False(t, rec.ReleasePending)
	assert.Equal(t, uint64(2), rec.Generation)
}

// The retry proves the stop from the launch record, not a pidfile: a pending
// launch whose recorded process still runs, or another node's, is held.
func TestMgmtReleaseReaper_HoldsWithoutProofOfStop(t *testing.T) {
	cases := map[string]func(t *testing.T, r *mgmtRig){
		"recorded process still running": func(t *testing.T, r *mgmtRig) {
			r.process(t, 4001, 'S', 1, "")
		},
		"pidfile holder still running": func(t *testing.T, r *mgmtRig) {
			r.process(t, 4999, 'S', 9, "i-1")
		},
		"another node's record": func(t *testing.T, r *mgmtRig) {
			rec := r.record(t, "i-1")
			rec.Node = "node-b"
			require.NoError(t, writeMgmtLaunch(r.dir, rec))
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			r := newMgmtRig(t)
			pendingRelease(t, r)
			setup(t, r)

			n, err := r.m.NewMgmtReleaseReaper().Sweep(t.Context())

			require.NoError(t, err)
			assert.Zero(t, n)
			assert.Equal(t, "i-1", r.addrs.held[mgmtTestIP])
			assert.True(t, r.record(t, "i-1").ReleasePending)
		})
	}
}

// A stop or terminate interrupted by a restart gives the address back when
// restore finishes it; a drained guest keeps its address for the relaunch.
func TestFinalizeTransitionalRestore_ReleasesTheInterruptedLaunchsAddress(t *testing.T) {
	cases := []struct {
		name     string
		status   InstanceState
		desired  DesiredState
		released bool
	}{
		{"stop", StateStopping, DesiredStopped, true},
		{"terminate", StateShuttingDown, DesiredRunning, true},
		{"drain", StateStopping, DesiredRunning, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newMgmtRig(t)
			r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 1, State: MgmtLaunchRunning, PID: 4001, StartTime: 1})
			r.addrs.held[mgmtTestIP] = "i-1"
			v := mgmtVM("i-1")
			v.Status, v.DesiredState = tc.status, tc.desired
			r.m.Insert(v)
			r.m.bindMgmtLaunch(v)

			r.m.finalizeTransitionalRestore(v)

			_, held := r.addrs.held[mgmtTestIP]
			assert.Equal(t, !tc.released, held)
		})
	}
}

// A stop whose launch is not yet proven stopped still records the release, so
// the retry gives the address back once the process has gone.
func TestReleaseMgmt_UnprovenStopIsReleasedOnceProven(t *testing.T) {
	r := newMgmtRig(t)
	r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 1, State: MgmtLaunchRunning, PID: 4001, StartTime: 1})
	r.addrs.held[mgmtTestIP] = "i-1"
	r.process(t, 4001, 'S', 1, "i-1")

	require.True(t, r.m.releaseMgmt(mgmtVM("i-1"), 1))
	require.Equal(t, "i-1", r.addrs.held[mgmtTestIP], "an unproven stop keeps the address")
	require.True(t, r.record(t, "i-1").ReleasePending)

	require.NoError(t, os.RemoveAll(filepath.Join(r.proc, strconv.Itoa(4001))))
	n, err := r.m.NewMgmtReleaseReaper().Sweep(t.Context())

	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Empty(t, r.addrs.held)
}

// A terminate that cannot write the release mark keeps the obligation in its
// teardown record; the teardown reaper records it later and the release follows.
func TestTerminate_UnrecordedReleaseIsKeptAsATeardownObligation(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a read-only directory does not stop root writing the launch record")
	}
	r := newCrashMgmtRig(t, &fakeVolumeMounter{})
	r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 1, State: MgmtLaunchRunning, PID: 4001, StartTime: 1})
	r.addrs.held[mgmtTestIP] = "i-1"
	v := mgmtVM("i-1")
	v.Status = StateShuttingDown
	v.mgmtGen.Store(1)
	r.m.Insert(v)
	require.NoError(t, os.Chmod(r.dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(r.dir, 0o700) })

	require.True(t, r.m.terminateCleanup(v, 1))

	assert.Equal(t, string(TeardownFailed), v.Teardown[TeardownMgmt])
	assert.False(t, r.record(t, "i-1").ReleasePending)
	assert.Equal(t, "i-1", r.addrs.held[mgmtTestIP])

	require.NoError(t, os.Chmod(r.dir, 0o700))
	results := r.m.NewTerminatedTeardownReaper().retryOutstanding(v)
	assert.Equal(t, TeardownDone, results[TeardownMgmt])
	assert.True(t, r.record(t, "i-1").ReleasePending)

	n, err := r.m.NewMgmtReleaseReaper().Sweep(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Empty(t, r.addrs.held)
}
