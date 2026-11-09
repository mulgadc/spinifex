// Package kvsync provides ownership of a key over a compare-and-swap store, in
// the manner of the standard sync package: a Claim is held until its holder
// unlocks it or another takes it over, and a Lease lapses unless renewed.
//
// Every acquisition returns a Token whose generation is the revision of the
// write that acquired the key. A store revision only grows, so a later holder's
// generation always exceeds an earlier one's, and a caller writing elsewhere
// fences on it there. Nothing in this package fences on its own.
package kvsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// KVStore is the compare-and-swap surface every kvsync lock is built on. A
// revision must only grow, including across a delete. The store value keys the
// registry of constructed locks, so it must be comparable, such as a pointer.
type KVStore interface {
	Load(ctx context.Context, key string) (value []byte, rev uint64, err error)
	CompareAndSwap(ctx context.Context, key string, value []byte, rev uint64) (uint64, error)
	CompareAndDelete(ctx context.Context, key string, rev uint64) error
}

var (
	// ErrNotFound is Load's report that no such key exists.
	ErrNotFound = errors.New("kvsync: key not found")
	// ErrRevisionMismatch reports that the key is not at the revision given;
	// revision 0 means the key must be absent.
	ErrRevisionMismatch = errors.New("kvsync: revision mismatch")
	// ErrUnavailable reports that the store could not be reached.
	ErrUnavailable = errors.New("kvsync: store unavailable")
	// ErrLost reports that the lock was taken over or lapsed.
	ErrLost = errors.New("kvsync: lock lost")
)

// Token names one acquisition of a key.
type Token struct {
	Holder     string
	Generation uint64
}

// HeldError reports that another holder has the key. Current is zero when the
// key was found absent after the conflict that produced this error.
type HeldError struct{ Current Token }

func (e *HeldError) Error() string {
	if e.Current.Holder == "" {
		return "kvsync: lock held by another holder"
	}
	return fmt.Sprintf("kvsync: lock held by %s at generation %d", e.Current.Holder, e.Current.Generation)
}

// record is the stored value. An acquiring write cannot know its own revision,
// so it stores generation 0, which a reader takes to mean the entry's revision.
type record struct {
	Holder     string `json:"holder"`
	Generation uint64 `json:"generation"`
}

func encode(holder string, generation uint64) []byte {
	data, _ := json.Marshal(record{Holder: holder, Generation: generation}) //nolint:errchkjson // A struct of a string and an integer cannot fail to marshal.
	return data
}

func decode(key string, value []byte, rev uint64) (Token, error) {
	var r record
	if err := json.Unmarshal(value, &r); err != nil {
		return Token{}, fmt.Errorf("kvsync: decode %s: %w", key, err)
	}
	if r.Holder == "" {
		return Token{}, fmt.Errorf("kvsync: decode %s: no holder", key)
	}
	if r.Generation == 0 {
		r.Generation = rev
	}
	return Token(r), nil
}

// read loads and decodes key. ok is false when the key is absent.
func read(ctx context.Context, store KVStore, key string) (tok Token, rev uint64, ok bool, err error) {
	value, rev, err := store.Load(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return Token{}, 0, false, nil
	}
	if err != nil {
		return Token{}, 0, false, fmt.Errorf("kvsync: load %s: %w", key, err)
	}
	tok, err = decode(key, value, rev)
	if err != nil {
		return Token{}, 0, false, err
	}
	return tok, rev, true, nil
}

// heldAfterConflict reports who has the key after a write lost its CAS.
func heldAfterConflict(ctx context.Context, store KVStore, key string) error {
	cur, _, _, err := read(ctx, store, key)
	if err != nil {
		return err
	}
	return &HeldError{Current: cur}
}

// casAttempts bounds the read-then-write loops that retry when the key changes
// between the read and the write.
const casAttempts = 3
