package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/mulgadc/spinifex/spinifex/vm"
)

const (
	// LocalStateSchemaVersion guards on-disk compatibility; bump on breaking changes.
	LocalStateSchemaVersion = 1

	// DefaultLocalStateDir is the default directory under DataDir for local state files.
	DefaultLocalStateDir = "state"

	// LocalStateFileName is the on-disk filename for per-node instance state.
	LocalStateFileName = "instance-state.json"

	// DefaultDataDir is the fallback DataDir when config.DataDir is empty.
	DefaultDataDir = "/var/lib/spinifex"
)

// LocalState is the on-disk representation of a node's instance state.
// SchemaVersion gates compatibility; unknown versions are rejected.
type LocalState struct {
	SchemaVersion int               `json:"schema_version"`
	VMS           map[string]*vm.VM `json:"vms"`
}

// LocalStatePath returns the absolute path to the per-node instance state file.
// Empty dataDir falls back to DefaultDataDir.
func LocalStatePath(dataDir string) string {
	if dataDir == "" {
		dataDir = DefaultDataDir
	}
	return filepath.Join(dataDir, DefaultLocalStateDir, LocalStateFileName)
}

// MarshalLocalState produces the JSON wire form of vms with schema version.
// Call inside a vm.Manager.View callback to avoid races on VM fields.
func MarshalLocalState(vms map[string]*vm.VM) ([]byte, error) {
	state := LocalState{
		SchemaVersion: LocalStateSchemaVersion,
		VMS:           vms,
	}
	data, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("marshal local state: %w", err)
	}
	return data, nil
}

// WriteLocalState atomically writes instance state via marshal → tmp → fsync →
// rename. vms must be a caller-owned snapshot; this function does not lock.
func WriteLocalState(path string, vms map[string]*vm.VM) error {
	data, err := MarshalLocalState(vms)
	if err != nil {
		return err
	}
	return WriteLocalStateBytes(path, data)
}

// WriteLocalStateBytes atomically writes pre-marshalled state JSON to path.
func WriteLocalStateBytes(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open tmp %s: %w", tmp, err)
	}

	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write tmp %s: %w", tmp, err)
	}

	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("fsync tmp %s: %w", tmp, err)
	}

	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close tmp %s: %w", tmp, err)
	}

	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}

	return nil
}

// ReadLocalState reads instance state from path. Returns (nil, nil) if the
// file does not exist (fresh install). Errors on JSON failure or unknown schema.
func ReadLocalState(path string) (*LocalState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Debug("Local state file missing, treating as fresh install", "path", path)
			return nil, nil
		}
		return nil, fmt.Errorf("read local state %s: %w", path, err)
	}

	var state LocalState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse local state %s: %w", path, err)
	}

	if state.SchemaVersion != LocalStateSchemaVersion {
		return nil, fmt.Errorf("local state %s: unknown schema_version %d (expected %d)",
			path, state.SchemaVersion, LocalStateSchemaVersion)
	}

	if state.VMS == nil {
		state.VMS = make(map[string]*vm.VM)
	}

	return &state, nil
}
