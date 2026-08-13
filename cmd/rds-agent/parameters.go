package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
)

// The generated parameter files and what it takes to install one. They live on
// the data volume at the paths rds-init writes: a class change replaces the VM,
// so a set under /etc would revert to defaults while the API reported otherwise.
type parameterStore struct {
	dir string
	// The include the resolved set is rendered to.
	installed string
	// The copy of the last set the engine accepted, which a boot loop is rolled
	// back to. Deliberately not a name the engine's own include glob reads, so it
	// is never parsed as a second set of settings.
	lastGood string
	// What the engine actually started on, for an engine that cannot be asked
	// which of its settings are still pending a restart. Empty for one that can.
	serving string
	// Prepended to every file written here, for a configuration format that has
	// no setting outside a group.
	header string
	// The engine parses its own configuration, so a root-owned file would be
	// unreadable to it.
	osUser string
	// Names the engine whose option-file spellings these files are written in.
	engine string
}

func (s parameterStore) installedPath() string { return filepath.Join(s.dir, s.installed) }
func (s parameterStore) lastGoodPath() string  { return filepath.Join(s.dir, s.lastGood) }
func (s parameterStore) servingPath() string   { return filepath.Join(s.dir, s.serving) }

// Byte-for-byte what rds-init writes for the same set, so the copies compared
// against it are comparing values rather than formatting.
func (s parameterStore) render(params []handlers_rds.Parameter) ([]byte, error) {
	body, err := renderParameters(s.engine, params)
	if err != nil {
		return nil, err
	}
	return []byte(s.header + body), nil
}

// Installs the resolved set at the path rds-init uses, so a later boot
// overwrites rather than shadows it. Returns a func putting back whatever was
// there, so the caller can undo the install without re-rendering anything.
func (s parameterStore) install(params []handlers_rds.Parameter) (restore func() error, err error) {
	path := s.installedPath()
	// A missing file is the first-ever apply, and its rollback is a removal.
	previous, readErr := os.ReadFile(path)
	if readErr != nil && !os.IsNotExist(readErr) {
		return nil, fmt.Errorf("read the installed parameters at %s: %w", path, readErr)
	}
	existed := readErr == nil

	rendered, err := s.render(params)
	if err != nil {
		return nil, err
	}
	if err := s.write(path, rendered); err != nil {
		return nil, err
	}
	return func() error {
		if !existed {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("withdraw the rejected parameters at %s: %w", path, err)
			}
			return nil
		}
		return s.write(path, previous)
	}, nil
}

// Atomic: a temp file in the same directory, renamed over the target. The temp
// name deliberately does not end in the engine's include suffix, so a crash
// between write and rename cannot leave the engine reading a half-written file.
func (s parameterStore) write(path string, content []byte) error {
	tmp := path + ".new"
	if err := os.WriteFile(tmp, content, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := s.chownToEngine(tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("install %s: %w", path, err)
	}
	return nil
}

// A guest with no engine user is a broken image, not something to work around.
func (s parameterStore) chownToEngine(path string) error {
	credential, err := lookupCredential(s.osUser)
	if err != nil {
		return err
	}
	if err := os.Chown(path, int(credential.Uid), int(credential.Gid)); err != nil {
		return fmt.Errorf("hand %s to %s: %w", path, s.osUser, err)
	}
	return nil
}

// Snapshots the installed set as the rollback target, but only once the engine
// is running all of it. A set still pending a restart has not been shown to
// serve, so promoting it would point the rollback at a config about to fail.
func recordLastGood(ctx context.Context, store parameterStore, pending pendingRestartFn) error {
	if _, err := os.Stat(store.installedPath()); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect the serving parameters: %w", err)
	}
	names, err := pending(ctx)
	if err != nil {
		return err
	}
	if len(names) > 0 {
		return nil
	}

	content, err := os.ReadFile(store.installedPath())
	if err != nil {
		return fmt.Errorf("read the serving parameters: %w", err)
	}
	lastGood, err := os.ReadFile(store.lastGoodPath())
	if err == nil && bytes.Equal(content, lastGood) {
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read the last known good parameters: %w", err)
	}
	return store.write(store.lastGoodPath(), content)
}

// Puts the last set the engine accepted back in place, for a restart that failed
// after a parameter change. The probe is checked again under the caller's
// parameter lock, so a repair that just brought the engine back is not reversed.
func restoreLastGood(ctx context.Context, store parameterStore, probe *engineProbe) (bool, error) {
	if state, _ := probe.state(ctx); state != engineAbsent {
		return false, nil
	}
	lastGood, err := os.ReadFile(store.lastGoodPath())
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read the last known good parameters: %w", err)
	}
	current, err := os.ReadFile(store.installedPath())
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("read the installed parameters: %w", err)
	}
	if bytes.Equal(current, lastGood) {
		return false, nil
	}
	if err := store.write(store.installedPath(), lastGood); err != nil {
		return false, err
	}
	return true, nil
}

// The settings the engine has accepted but will not honour until it restarts.
// Each implementation answers it its own way, and both the apply and the serving
// snapshot are gated on the same answer.
type pendingRestartFn func(ctx context.Context) ([]string, error)

// The parameter-file state every engine keeps. The operations over it are free
// functions above rather than methods here, so each implementation names its own
// restart and pending answers at the call site where the compiler checks them.
type parameterManager struct {
	params parameterStore
	probe  *engineProbe
	// Serializes parameter installs, serving snapshots and rollback restores so
	// none can copy or replace an intermediate configuration.
	paramMu       sync.Mutex
	repairTimeout time.Duration
	repairPoll    time.Duration
}

const (
	parameterRepairTimeout = 90 * time.Second
	parameterRepairPoll    = time.Second
)

// Starts a down engine on the set just installed and waits for it to serve.
// Reached when the engine went away during an apply: the installed set is the
// newer, so starting on it beats restoring one that already failed.
func awaitRepairedEngine(ctx context.Context, probe *engineProbe, restart func(context.Context) error,
	pending pendingRestartFn, timeout, poll time.Duration) ([]string, error) {
	repairCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := restart(repairCtx); err != nil {
		return nil, fmt.Errorf("start the engine on the repaired parameter set: %w", err)
	}

	timer := time.NewTimer(0)
	defer timer.Stop()
	lastMessage := "engine did not respond"
	for {
		select {
		case <-repairCtx.Done():
			if ctx.Err() != nil {
				return nil, fmt.Errorf("wait for the engine on the repaired parameter set: %w", ctx.Err())
			}
			return nil, fmt.Errorf("wait for the engine on the repaired parameter set: %s: %w", lastMessage, repairCtx.Err())
		case <-timer.C:
		}

		state, message := probe.state(repairCtx)
		if state == engineServing {
			names, err := pending(repairCtx)
			if err == nil {
				return names, nil
			}
			lastMessage = err.Error()
		} else if message != "" {
			lastMessage = message
		}
		timer.Reset(poll)
	}
}
