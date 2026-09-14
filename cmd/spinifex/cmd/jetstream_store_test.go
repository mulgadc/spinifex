//test:in-package: swaps the unexported procRoot for a fake /proc tree and asserts on
// the store helpers the init and join commands call.

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withFakeProc points the process scan at a temp tree holding the given
// pid -> cmdline entries (NUL-separated argv) for the duration of the test.
func withFakeProc(t *testing.T, procs map[string]string) {
	t.Helper()
	root := t.TempDir()
	for pid, cmdline := range procs {
		dir := filepath.Join(root, pid)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline), 0o644))
	}
	prev := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = prev })
}

// seedStore builds a JetStream store with the named streams under account $G,
// laid out the way nats-server writes it, and returns the store path.
func seedStore(t *testing.T, streams ...string) string {
	t.Helper()
	store := filepath.Join(t.TempDir(), "nats", "jetstream")
	for _, s := range streams {
		dir := filepath.Join(store, "$G", "streams", s, "msgs")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "1.blk"), []byte("x"), 0o600))
	}
	return store
}

func TestJetStreamStoreDir(t *testing.T) {
	assert.Equal(t, "/var/lib/spinifex/nats/jetstream", jetStreamStoreDir("/var/lib/spinifex"))
}

func TestLocalStreams(t *testing.T) {
	t.Run("missing store holds nothing", func(t *testing.T) {
		got, err := localStreams(filepath.Join(t.TempDir(), "absent"))
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("lists streams across accounts, sorted", func(t *testing.T) {
		store := seedStore(t, "KV_b", "KV_a")
		require.NoError(t, os.MkdirAll(filepath.Join(store, "$SYS", "_js_", "_meta_"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(store, "$G", "streams", "stray-file"), nil, 0o600))

		got, err := localStreams(store)
		require.NoError(t, err)
		assert.Equal(t, []string{"$G/KV_a", "$G/KV_b"}, got)
	})
}

func TestNATSRunningLocally(t *testing.T) {
	tests := []struct {
		name  string
		procs map[string]string
		want  bool
	}{
		{"no processes", nil, false},
		{"installed service", map[string]string{"10": "/usr/local/bin/spx\x00service\x00nats\x00start\x00"}, true},
		{"bare spx on PATH", map[string]string{"10": "spx\x00service\x00nats\x00start\x00"}, true},
		// The join itself is an spx process and must not count.
		{"spx admin join", map[string]string{"10": "spx\x00admin\x00join\x00--force\x00"}, false},
		{"other spx service", map[string]string{"10": "spx\x00service\x00predastore\x00start\x00"}, false},
		{"nats-server binary", map[string]string{"10": "nats-server\x00service\x00nats\x00"}, false},
		{"kernel thread, empty cmdline", map[string]string{"2": ""}, false},
		{"non-pid entry ignored", map[string]string{"self": "spx\x00service\x00nats\x00"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withFakeProc(t, tt.procs)
			got, err := natsRunningLocally()
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDiscardJetStreamStore(t *testing.T) {
	t.Run("removes the store", func(t *testing.T) {
		withFakeProc(t, nil)
		store := seedStore(t, "KV_a")
		require.NoError(t, discardJetStreamStore(store))
		_, err := os.Stat(store)
		assert.ErrorIs(t, err, os.ErrNotExist)
	})

	t.Run("missing store is not an error", func(t *testing.T) {
		withFakeProc(t, nil)
		assert.NoError(t, discardJetStreamStore(filepath.Join(t.TempDir(), "absent")))
	})

	t.Run("refuses under a running NATS and leaves the store", func(t *testing.T) {
		withFakeProc(t, map[string]string{"7": "spx\x00service\x00nats\x00start\x00"})
		store := seedStore(t, "KV_a")
		assert.ErrorIs(t, discardJetStreamStore(store), errNATSRunning)
		_, err := os.Stat(store)
		assert.NoError(t, err)
	})
}

func TestCheckInitJetStreamStore(t *testing.T) {
	withFakeProc(t, nil)

	t.Run("single-node init keeps its store", func(t *testing.T) {
		assert.NoError(t, checkInitJetStreamStore(seedStore(t, "KV_a"), 1, false))
	})

	t.Run("multi-node init over an empty store", func(t *testing.T) {
		assert.NoError(t, checkInitJetStreamStore(filepath.Join(t.TempDir(), "absent"), 3, false))
	})

	t.Run("multi-node init over streams is refused without the flag", func(t *testing.T) {
		err := checkInitJetStreamStore(seedStore(t, "KV_a", "KV_b"), 4, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "2 stream(s)")
		assert.Contains(t, err.Error(), "--discard-jetstream")
	})

	t.Run("multi-node init over streams proceeds with the flag", func(t *testing.T) {
		assert.NoError(t, checkInitJetStreamStore(seedStore(t, "KV_a"), 4, true))
	})

	t.Run("flag does not override a running NATS", func(t *testing.T) {
		withFakeProc(t, map[string]string{"7": "spx\x00service\x00nats\x00start\x00"})
		assert.ErrorIs(t, checkInitJetStreamStore(seedStore(t, "KV_a"), 4, true), errNATSRunning)
	})
}
