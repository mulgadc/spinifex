// Package workerkey keeps the SQS guest's receipt-signing key on its own
// persistent disk. The key is generated and held only inside the guest; the
// platform learns its fingerprint alone.
package workerkey

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

const (
	tempPrefix = ".receipt-"
	tempSuffix = ".tmp"
)

// Each refusal is fail-closed: the guest never replaces a key it cannot use.
var (
	ErrKeyMismatch = errors.New("worker receipt key does not match the recorded fingerprint")
	ErrUnsafe      = errors.New("worker receipt key path is not owner-only")
)

// Store is the key directory on the worker disk. Workerkey cannot tell a
// mounted disk from an empty mountpoint, so the caller must first verify the
// expected data volume is mounted there.
type Store struct {
	Dir string
}

// prepareDir creates only the key directory, inside a parent that must already
// exist, refuses one that is not owner-only, locks it and removes temporary
// files a crash left behind.
func (s Store) prepareDir() (unlock func(), err error) {
	if s.Dir == "" {
		return nil, errors.New("worker receipt key directory is not set")
	}
	parent, err := os.Stat(filepath.Dir(s.Dir))
	if err != nil || !parent.IsDir() {
		return nil, errors.New("worker key directory parent is missing")
	}
	if err := os.Mkdir(s.Dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("create worker key directory: %w", err)
	}
	fi, err := os.Lstat(s.Dir)
	if err != nil {
		return nil, fmt.Errorf("stat worker key directory: %w", err)
	}
	if !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 {
		return nil, ErrUnsafe
	}
	d, err := os.Open(s.Dir)
	if err != nil {
		return nil, fmt.Errorf("open worker key directory: %w", err)
	}
	if err := syscall.Flock(int(d.Fd()), syscall.LOCK_EX); err != nil {
		d.Close()
		return nil, fmt.Errorf("lock worker key directory: %w", err)
	}
	unlock = func() { d.Close() }
	stale, err := filepath.Glob(filepath.Join(s.Dir, tempPrefix+"*"+tempSuffix))
	if err != nil {
		unlock()
		return nil, fmt.Errorf("list stale worker key files: %w", err)
	}
	for _, f := range stale {
		if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			unlock()
			return nil, fmt.Errorf("remove stale worker key file: %w", err)
		}
	}
	return unlock, nil
}

// syncNames makes the key's directory entry and the key directory's own entry
// in its parent durable.
func (s Store) syncNames() error {
	if err := syncDirFn(s.Dir); err != nil {
		return err
	}
	return syncDirFn(filepath.Dir(s.Dir))
}

func writeSynced(f *os.File, data []byte) error {
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return fmt.Errorf("restrict worker receipt key file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write worker receipt key file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync worker receipt key file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close worker receipt key file: %w", err)
	}
	return nil
}

// syncDirFn is replaced in tests to fail a directory sync deterministically.
var syncDirFn = syncDir

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open worker key directory: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync worker key directory: %w", err)
	}
	return nil
}
