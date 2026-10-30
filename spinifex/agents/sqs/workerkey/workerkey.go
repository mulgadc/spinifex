// Package workerkey keeps the shared SQS worker's store-client NKey on the
// worker's own persistent disk. The seed is generated and held only inside the
// worker guest; the platform learns the public key alone.
package workerkey

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/nats-io/nkeys"
)

const (
	keyFile    = "store-client.nk"
	tempPrefix = ".store-client-"
	tempSuffix = ".tmp"
	// A user seed is 58 bytes; anything much larger is not one.
	maxKeyFileSize = 256
)

// Each refusal is fail-closed: the worker never replaces a key it cannot use.
var (
	ErrKeyMissing  = errors.New("worker store key is missing but the store already exists")
	ErrKeyMismatch = errors.New("worker store key does not match the store's authorized key")
	ErrUnsafe      = errors.New("worker store key path is not owner-only")
	ErrCorrupt     = errors.New("worker store key file does not hold a user NKey seed")
)

// Store is the key directory on the mounted worker disk.
type Store struct {
	Dir string
}

// Ensure returns the worker's store-client key pair. An empty expected means no
// store exists yet, so a missing key is generated and synced before it is
// returned; a non-empty expected requires the existing key to match it.
func (s Store) Ensure(expected string) (nkeys.KeyPair, error) {
	if expected != "" && !nkeys.IsValidPublicUserKey(expected) {
		return nil, errors.New("expected store key is not a user public NKey")
	}
	unlock, err := s.prepareDir()
	if err != nil {
		return nil, err
	}
	defer unlock()
	kp, err := s.load()
	if errors.Is(err, fs.ErrNotExist) {
		if expected != "" {
			return nil, ErrKeyMissing
		}
		kp, err = s.create()
	}
	if err != nil {
		return nil, err
	}
	if expected != "" {
		pub, err := kp.PublicKey()
		if err != nil {
			kp.Wipe()
			return nil, ErrCorrupt
		}
		if pub != expected {
			kp.Wipe()
			return nil, fmt.Errorf("%w: have %s, store authorizes %s", ErrKeyMismatch, pub, expected)
		}
	}
	return kp, nil
}

func (s Store) path() string { return filepath.Join(s.Dir, keyFile) }

// prepareDir creates the directory owner-only, refuses one that is not, takes
// an exclusive lock on it and removes temporary files a crash left behind.
func (s Store) prepareDir() (unlock func(), err error) {
	if s.Dir == "" {
		return nil, errors.New("worker store key directory is not set")
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
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

// load reads an existing key. Any file that is present but unusable refuses
// rather than being replaced.
func (s Store) load() (nkeys.KeyPair, error) {
	fi, err := os.Lstat(s.path())
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
		return nil, ErrUnsafe
	}
	if fi.Size() > maxKeyFileSize {
		return nil, ErrCorrupt
	}
	seed, err := os.ReadFile(s.path())
	if err != nil {
		return nil, fmt.Errorf("read worker store key: %w", err)
	}
	kp, err := nkeys.FromSeed(bytes.TrimSpace(seed))
	if err != nil {
		return nil, ErrCorrupt
	}
	pub, err := kp.PublicKey()
	if err != nil || !nkeys.IsValidPublicUserKey(pub) {
		kp.Wipe()
		return nil, ErrCorrupt
	}
	return kp, nil
}

// create writes a new seed to a synced temporary file, links it into place
// without replacing anything, then syncs the directory so the name survives.
func (s Store) create() (nkeys.KeyPair, error) {
	kp, err := nkeys.CreateUser()
	if err != nil {
		return nil, fmt.Errorf("generate worker store key: %w", err)
	}
	seed, err := kp.Seed()
	if err != nil {
		kp.Wipe()
		return nil, fmt.Errorf("encode worker store key: %w", err)
	}

	tmp, err := os.CreateTemp(s.Dir, tempPrefix+"*"+tempSuffix)
	if err != nil {
		kp.Wipe()
		return nil, fmt.Errorf("create worker store key file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := writeSynced(tmp, seed); err != nil {
		kp.Wipe()
		return nil, err
	}
	if err := os.Link(tmpName, s.path()); err != nil {
		kp.Wipe()
		if errors.Is(err, fs.ErrExist) {
			return s.load()
		}
		return nil, fmt.Errorf("install worker store key file: %w", err)
	}
	if err := syncDir(s.Dir); err != nil {
		kp.Wipe()
		return nil, err
	}
	return kp, nil
}

func writeSynced(f *os.File, data []byte) error {
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return fmt.Errorf("restrict worker store key file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write worker store key file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync worker store key file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close worker store key file: %w", err)
	}
	return nil
}

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
