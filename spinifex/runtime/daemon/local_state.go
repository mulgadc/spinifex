package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
)

const (
	// LocalStateSchemaVersion is the newest format for a node's instance state
	// record, on disk and in KV. Version 2 adds persisted block-export fields.
	// An N-1 daemon must reject a v2 record rather than silently discard them.
	LocalStateSchemaVersion = 2

	// legacyLocalStateSchemaVersion remains the emitted form until state carries
	// a block export. That keeps the inactive expansion rollback-compatible.
	legacyLocalStateSchemaVersion = 1

	// DefaultLocalStateDir is the default directory under DataDir for local state files.
	DefaultLocalStateDir = "state"

	// LocalStateFileName is the on-disk filename for per-node instance state.
	LocalStateFileName = "instance-state.json"

	// DefaultDataDir is the fallback DataDir when config.DataDir is empty.
	DefaultDataDir = "/var/lib/spinifex"
)

// LocalState is a node's instance state, both on disk and as the node.<id>
// record in the instance-state KV bucket. SchemaVersion gates compatibility;
// unknown versions are rejected.
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

// MarshalLocalState produces the JSON wire form of vms with the lowest schema
// that preserves them. Call inside a vm.Manager.View callback to avoid races
// on VM fields.
func MarshalLocalState(vms map[string]*vm.VM) ([]byte, error) {
	state := LocalState{
		SchemaVersion: localStateSchemaVersion(vms),
		VMS:           vms,
	}
	data, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("marshal local state: %w", err)
	}
	return data, nil
}

func localStateSchemaVersion(vms map[string]*vm.VM) int {
	for _, instance := range vms {
		for _, request := range instance.EBSRequests.Requests {
			if request.BlockDevice != "" || request.BlockSizeBytes != 0 {
				return LocalStateSchemaVersion
			}
		}
	}
	return legacyLocalStateSchemaVersion
}

// LocalStateFileMode is the mode of the on-disk instance state file. The daemon
// writes it and vpcd reads it to serve IMDS; they run as different users in the
// shared spinifex group, so owner-only would deny every IMDS local lookup.
const LocalStateFileMode = 0o640

// WriteLocalStateBytes atomically writes pre-marshalled state JSON to path.
func WriteLocalStateBytes(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, LocalStateFileMode)
	if err != nil {
		return fmt.Errorf("open tmp %s: %w", tmp, err)
	}

	// Explicitly, because OpenFile's mode is masked by the umask the service
	// happens to inherit, and a 0600 here silently costs IMDS its local lookup.
	if err := f.Chmod(LocalStateFileMode); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("chmod tmp %s: %w", tmp, err)
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
// file does not exist (fresh install). It accepts both deployed schemas and
// rejects every other version, including a future format it cannot preserve.
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

	// The file has carried a version since it was introduced, so 0 is
	// corruption rather than an old record. Version 1 had the same enclosing
	// representation and simply lacks block-export fields, making this an
	// idempotent expansion.
	switch state.SchemaVersion {
	case legacyLocalStateSchemaVersion:
	case LocalStateSchemaVersion:
		// Current form.
	default:
		return nil, fmt.Errorf("local state %s: unknown schema_version %d (supported versions are %d and %d)",
			path, state.SchemaVersion, legacyLocalStateSchemaVersion, LocalStateSchemaVersion)
	}

	if state.VMS == nil {
		state.VMS = make(map[string]*vm.VM)
	}

	return &state, nil
}
