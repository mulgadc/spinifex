package vpcd_test

import (
	"os"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/daemon"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/mulgadc/spinifex/spinifex/vpcd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeInstanceState(t *testing.T, dataDir string, vms map[string]*vm.VM) {
	t.Helper()
	data, err := daemon.MarshalLocalState(vms)
	require.NoError(t, err)
	require.NoError(t, daemon.WriteLocalStateBytes(daemon.LocalStatePath(dataDir), data))
}

func TestLocalVMStateReader_MissingFileIsAMiss(t *testing.T) {
	r := vpcd.NewLocalVMStateReaderForTest(t.TempDir(), nil)
	v, err := r.LocalVM("i-absent")
	require.NoError(t, err)
	assert.Nil(t, v)
}

func TestLocalVMStateReader_ResolvesAKnownInstance(t *testing.T) {
	dataDir := t.TempDir()
	writeInstanceState(t, dataDir, map[string]*vm.VM{"i-a": {ID: "i-a", InstanceType: "t3.micro"}})

	r := vpcd.NewLocalVMStateReaderForTest(dataDir, nil)
	v, err := r.LocalVM("i-a")
	require.NoError(t, err)
	require.NotNil(t, v)
	assert.Equal(t, "t3.micro", v.InstanceType)
}

func TestLocalVMStateReader_CachesParseUntilFileChanges(t *testing.T) {
	dataDir := t.TempDir()
	writeInstanceState(t, dataDir, map[string]*vm.VM{"i-a": {ID: "i-a"}})

	parses := 0
	r := vpcd.NewLocalVMStateReaderForTest(dataDir, func(path string) (*daemon.LocalState, error) {
		parses++
		return daemon.ReadLocalState(path)
	})

	_, err := r.LocalVM("i-a")
	require.NoError(t, err)
	_, err = r.LocalVM("i-a")
	require.NoError(t, err)
	assert.Equal(t, 1, parses, "a second lookup on an unchanged file must reuse the cached parse")

	writeInstanceState(t, dataDir, map[string]*vm.VM{"i-a": {ID: "i-a"}, "i-b": {ID: "i-b"}})
	_, err = r.LocalVM("i-b")
	require.NoError(t, err)
	assert.Equal(t, 2, parses, "a changed file must be re-parsed")
}

// TestLocalVMStateReader_CorruptFileDoesNotServeStaleCache proves a stat/parse
// failure clears the cache and surfaces the error rather than replaying the
// last good parse — the caller (localInstanceLookup) treats this as a
// fall-through to the record space, not a silently stale answer.
func TestLocalVMStateReader_CorruptFileDoesNotServeStaleCache(t *testing.T) {
	dataDir := t.TempDir()
	writeInstanceState(t, dataDir, map[string]*vm.VM{"i-a": {ID: "i-a"}})

	r := vpcd.NewLocalVMStateReaderForTest(dataDir, nil)
	v, err := r.LocalVM("i-a")
	require.NoError(t, err)
	require.NotNil(t, v, "warm the cache with a good parse")

	// Different size than the good state, so the cache cannot mistake this
	// for an unchanged file and must attempt (and fail) a re-parse.
	require.NoError(t, os.WriteFile(daemon.LocalStatePath(dataDir), []byte("not json"), 0o600))

	_, err = r.LocalVM("i-a")
	assert.Error(t, err, "a corrupt state file must surface the read failure, not the last good parse")
}
