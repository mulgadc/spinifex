package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocalStatePath_UsesDataDir(t *testing.T) {
	got := LocalStatePath("/data")
	assert.Equal(t, "/data/state/instance-state.json", got)
}

func TestLocalStatePath_DefaultWhenEmpty(t *testing.T) {
	got := LocalStatePath("")
	assert.Equal(t, "/var/lib/spinifex/state/instance-state.json", got)
}

func TestWriteLocalState_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "instance-state.json")

	vms := map[string]*vm.VM{
		"i-aaa": {ID: "i-aaa", InstanceType: "t3.micro"},
		"i-bbb": {ID: "i-bbb", InstanceType: "m5.large"},
	}

	require.NoError(t, WriteLocalState(path, vms))

	loaded, err := ReadLocalState(path)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, LocalStateSchemaVersion, loaded.SchemaVersion)
	assert.Len(t, loaded.VMS, 2)
	assert.Equal(t, "t3.micro", loaded.VMS["i-aaa"].InstanceType)
	assert.Equal(t, "m5.large", loaded.VMS["i-bbb"].InstanceType)
}

func TestWriteLocalState_MkdirParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deep", "nested", "state", "instance-state.json")
	require.NoError(t, WriteLocalState(path, map[string]*vm.VM{}))
	_, err := os.Stat(path)
	require.NoError(t, err)
}

func TestWriteLocalState_NoTmpLeftBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "instance-state.json")
	require.NoError(t, WriteLocalState(path, map[string]*vm.VM{}))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".tmp", "stale tmp file left behind")
	}
}

func TestWriteLocalState_AtomicReplace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance-state.json")

	require.NoError(t, WriteLocalState(path, map[string]*vm.VM{
		"i-1": {ID: "i-1"},
	}))

	require.NoError(t, WriteLocalState(path, map[string]*vm.VM{
		"i-2": {ID: "i-2"},
	}))

	loaded, err := ReadLocalState(path)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Len(t, loaded.VMS, 1)
	_, ok := loaded.VMS["i-2"]
	assert.True(t, ok, "second write should replace, not merge")
}

func TestReadLocalState_Missing(t *testing.T) {
	state, err := ReadLocalState(filepath.Join(t.TempDir(), "absent.json"))
	require.NoError(t, err)
	assert.Nil(t, state, "missing file is the fresh-install signal")
}

func TestReadLocalState_CorruptJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance-state.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	_, err := ReadLocalState(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse local state")
}

func TestReadLocalState_UnknownSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance-state.json")
	data, _ := json.Marshal(map[string]any{"schema_version": 99, "vms": map[string]any{}})
	require.NoError(t, os.WriteFile(path, data, 0o600))

	_, err := ReadLocalState(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown schema_version 99")
}

func TestReadLocalState_NilVMSMapInitialized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance-state.json")
	data, _ := json.Marshal(map[string]any{"schema_version": LocalStateSchemaVersion})
	require.NoError(t, os.WriteFile(path, data, 0o600))

	state, err := ReadLocalState(path)
	require.NoError(t, err)
	require.NotNil(t, state)
	assert.NotNil(t, state.VMS)
}
