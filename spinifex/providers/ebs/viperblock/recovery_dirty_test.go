package viperblock

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	viperblocklegacyv1 "github.com/mulgadc/spinifex/contracts/viperblockd/legacy/v1"
	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRecoverMountedVolumes_StartupOrderMarksTheAdoptedExport follows Run's
// order from a cold Config: InitLeases, then recovery. An export adopted
// before the markers existed ran with no record of its unsealed writes.
func TestRecoverMountedVolumes_StartupOrderMarksTheAdoptedExport(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	cfg.leases, cfg.dirty = nil, nil
	cfg.constructVB = fileBackedConstructVB(t)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	require.NoError(t, cfg.InitLeases(t.Context(), nc))

	const volumeName = "vol-recoverymarked1"
	procRoot := t.TempDir()
	socket := filepath.Join(t.TempDir(), "m.sock")
	listenUnix(t, socket)
	writeProcFixture(t, procRoot, 434343, "nbdkit", nbdkitArgv(cfg.BaseDir, socket, 0, volumeName))

	recoverMountedVolumes(t.Context(), cfg, nc, procRoot)

	mv, ok := findMountedVolume(cfg, volumeName)
	require.True(t, ok)
	t.Cleanup(func() { mv.VB.StopWALSyncer() })

	record, found := cfg.dirty.holder(t.Context(), volumeName)
	require.True(t, found, "an adopted export holds unsealed writes until it seals, so it must be marked")
	assert.Equal(t, cfg.leaseOwner(), record.Owner)
	assert.Equal(t, mv.leaseGeneration(), record.Generation)
}

// TestRecoverMountedVolumes_UnrecordableAdoptLeavesAnInUseExportRunning covers
// recovery that cannot record the export's writes. It must not adopt it, and a
// guest still using the export must keep it: refusing is not a reason to kill.
func TestRecoverMountedVolumes_UnrecordableAdoptLeavesAnInUseExportRunning(t *testing.T) {
	cases := map[string]func(t *testing.T, cfg *Config, js jetstream.JetStream){
		"no marker store": func(_ *testing.T, cfg *Config, _ jetstream.JetStream) { cfg.dirty = nil },
		"marker write fails": func(t *testing.T, _ *Config, js jetstream.JetStream) {
			require.NoError(t, js.DeleteKeyValue(t.Context(), viperblocklegacyv1.DirtyBucket))
		},
	}
	for name, breakMarkers := range cases {
		t.Run(name, func(t *testing.T) {
			_, natsURL := setupEmbeddedNATS(t)
			cfg := setupTestConfig(t, natsURL)
			cfg.constructVB = fileBackedConstructVB(t)

			nc, err := nats.Connect(natsURL)
			require.NoError(t, err)
			t.Cleanup(nc.Close)
			js, err := jetstream.New(nc)
			require.NoError(t, err)
			breakMarkers(t, cfg, js)

			const volumeName = "vol-recoveryunmarked1"
			procRoot := t.TempDir()
			pid := spawnLongRunningProcess(t)
			socket := filepath.Join(t.TempDir(), "u.sock")
			listenUnix(t, socket)
			writeProcFixture(t, procRoot, pid, "nbdkit", nbdkitArgv(cfg.BaseDir, socket, 0, volumeName))
			writeReferencerFixture(t, procRoot, 919191,
				fmt.Sprintf("driver=nbd,node-name=vol0,server.type=unix,server.path=%s,export=", socket))

			recoverMountedVolumes(context.Background(), cfg, nc, procRoot)

			_, ok := findMountedVolume(cfg, volumeName)
			assert.False(t, ok, "an export whose writes cannot be recorded must not be adopted")
			assert.Never(t, func() bool { return !hostprocess.ProcessAlive(pid) }, 100*time.Millisecond, 10*time.Millisecond,
				"an export a guest is still using must never be signalled")
		})
	}
}
