package workerkey

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	receiptFile = "receipt.key"
	// ReceiptKeyLen is the receipt-signing key's length in bytes.
	ReceiptKeyLen = 32
)

// ErrReceiptKeyMissing is a worker disk without the receipt-signing key. It
// is created only with the store key for a new store, so it is never
// regenerated: a new key would invalidate every handle already issued.
var ErrReceiptKeyMissing = errors.New("worker receipt-signing key is missing")

func (s Store) receiptPath() string { return filepath.Join(s.Dir, receiptFile) }

// ReceiptKey returns the receipt-signing key already on disk; it never
// creates one. The caller must not log or retain it beyond use.
func (s Store) ReceiptKey() ([]byte, error) {
	unlock, err := s.prepareDir()
	if err != nil {
		return nil, err
	}
	defer unlock()
	key, err := s.loadReceipt()
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrReceiptKeyMissing
	}
	return key, err
}

// ensureReceipt creates the receipt-signing key if absent. Only the
// new-store path calls it, under the same decision that creates the store key.
func (s Store) ensureReceipt() error {
	unlock, err := s.prepareDir()
	if err != nil {
		return err
	}
	defer unlock()
	key, err := s.loadReceipt()
	if err == nil {
		clear(key)
		return s.syncNames()
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	key = make([]byte, ReceiptKeyLen)
	defer clear(key)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("generate worker receipt key: %w", err)
	}
	tmp, err := os.CreateTemp(s.Dir, tempPrefix+"*"+tempSuffix)
	if err != nil {
		return fmt.Errorf("create worker receipt key file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := writeSynced(tmp, key); err != nil {
		return err
	}
	if err := os.Link(tmpName, s.receiptPath()); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("install worker receipt key file: %w", err)
	}
	return s.syncNames()
}

// loadReceipt refuses a file that is present but unusable rather than
// replacing it.
func (s Store) loadReceipt() ([]byte, error) {
	fi, err := os.Lstat(s.receiptPath())
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
		return nil, ErrUnsafe
	}
	if fi.Size() != ReceiptKeyLen {
		return nil, errors.New("worker receipt key file has the wrong length")
	}
	key, err := os.ReadFile(s.receiptPath())
	if err != nil {
		return nil, fmt.Errorf("read worker receipt key: %w", err)
	}
	if len(key) != ReceiptKeyLen {
		clear(key)
		return nil, errors.New("worker receipt key file has the wrong length")
	}
	return key, nil
}
