package workerkey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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

// ErrReceiptKeyMissing is a worker disk without the receipt-signing key once
// its fingerprint is recorded. It is never regenerated: a new key would
// invalidate every handle already issued.
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

// ensureReceipt loads the receipt-signing key, creating it only if absent.
// Only a pending initialization calls it, so a crash before the fingerprint
// is recorded resumes with the key already written.
func (s Store) ensureReceipt() ([]byte, error) {
	unlock, err := s.prepareDir()
	if err != nil {
		return nil, err
	}
	defer unlock()
	key, err := s.loadReceipt()
	if err == nil {
		if err := s.syncNames(); err != nil {
			clear(key)
			return nil, err
		}
		return key, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	key = make([]byte, ReceiptKeyLen)
	if _, err := rand.Read(key); err != nil {
		clear(key)
		return nil, fmt.Errorf("generate worker receipt key: %w", err)
	}
	if err := s.install(key); err != nil {
		clear(key)
		return nil, err
	}
	return key, nil
}

// install links a synced temporary file into place, so the key appears whole
// or not at all, then makes both directory entries durable.
func (s Store) install(key []byte) error {
	tmp, err := os.CreateTemp(s.Dir, tempPrefix+"*"+tempSuffix)
	if err != nil {
		return fmt.Errorf("create worker receipt key file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := writeSynced(tmp, key); err != nil {
		return err
	}
	if err := os.Link(tmpName, s.receiptPath()); err != nil {
		return fmt.Errorf("install worker receipt key file: %w", err)
	}
	return s.syncNames()
}

// Fingerprint is the SHA-256 of a receipt key, hex encoded. It identifies the
// key without revealing it.
func Fingerprint(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])
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
