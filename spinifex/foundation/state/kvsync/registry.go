package kvsync

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// registries holds, per store value, every key and prefix constructed against
// it, so two owners in one process can never share a key by accident.
var registries = struct {
	mu sync.Mutex
	m  map[KVStore]*keySet
}{m: map[KVStore]*keySet{}}

type keySet struct {
	keys     map[string]struct{}
	prefixes map[string]struct{}
}

// registerKey records key against store unless it is invalid, already
// registered, or under a registered prefix.
func registerKey(store KVStore, key string) error {
	if err := validKey(key); err != nil {
		return err
	}
	set, err := keySetFor(store)
	if err != nil {
		return err
	}
	defer registries.mu.Unlock()
	if _, dup := set.keys[key]; dup {
		return fmt.Errorf("kvsync: key %q constructed twice against one store", key)
	}
	for p := range set.prefixes {
		if strings.HasPrefix(key, p) {
			return fmt.Errorf("kvsync: key %q is under registered prefix %q", key, p)
		}
	}
	set.keys[key] = struct{}{}
	return nil
}

// registerPrefix records prefix against store unless it is invalid, overlaps a
// registered prefix, or covers a registered key.
func registerPrefix(store KVStore, prefix string) error {
	if err := validPrefix(prefix); err != nil {
		return err
	}
	set, err := keySetFor(store)
	if err != nil {
		return err
	}
	defer registries.mu.Unlock()
	for p := range set.prefixes {
		if strings.HasPrefix(prefix, p) || strings.HasPrefix(p, prefix) {
			return fmt.Errorf("kvsync: prefix %q overlaps registered prefix %q", prefix, p)
		}
	}
	for k := range set.keys {
		if strings.HasPrefix(k, prefix) {
			return fmt.Errorf("kvsync: prefix %q covers registered key %q", prefix, k)
		}
	}
	set.prefixes[prefix] = struct{}{}
	return nil
}

// keySetFor returns store's key set with registries.mu held; the caller unlocks.
func keySetFor(store KVStore) (*keySet, error) {
	if store == nil {
		return nil, errors.New("kvsync: nil store")
	}
	registries.mu.Lock()
	set, ok := registries.m[store]
	if !ok {
		set = &keySet{keys: map[string]struct{}{}, prefixes: map[string]struct{}{}}
		registries.m[store] = set
	}
	return set, nil
}

// mustWire panics on a construction error, the one place this package panics.
func mustWire(err error) {
	if err != nil {
		panic(err.Error()) //nolint:forbidigo // Constructing a lock wrongly is a wiring error that must fail at startup, not at the first election.
	}
}

func validHolder(holder string) error {
	if holder == "" {
		return errors.New("kvsync: empty holder")
	}
	return nil
}

func validTTL(ttl time.Duration) error {
	if ttl <= 0 {
		return errors.New("kvsync: lease ttl must be positive")
	}
	return nil
}

// validChars reports whether s holds only characters JetStream KV accepts in a key.
func validChars(s string) bool {
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '/', c == '_', c == '=', c == '.':
		default:
			return false
		}
	}
	return true
}

// validName checks the rules every key and group name shares: non-empty,
// characters JetStream accepts, and no empty dot-separated token.
func validName(kind, s string) error {
	switch {
	case s == "":
		return fmt.Errorf("kvsync: empty %s", kind)
	case !validChars(s):
		return fmt.Errorf("kvsync: %s %q has a character JetStream KV refuses", kind, s)
	case strings.HasPrefix(s, "."), strings.HasSuffix(s, "."), strings.Contains(s, ".."):
		return fmt.Errorf("kvsync: %s %q has an empty token", kind, s)
	}
	return nil
}

func validKey(key string) error {
	if err := validName("key", key); err != nil {
		return err
	}
	if strings.HasPrefix(key, "_") {
		return fmt.Errorf("kvsync: key %q starts with _, which the bucket's schema version key reserves", key)
	}
	return nil
}

func validPrefix(prefix string) error {
	if !strings.HasSuffix(prefix, ".") {
		return fmt.Errorf("kvsync: prefix %q must end in a dot", prefix)
	}
	return validKey(strings.TrimSuffix(prefix, "."))
}
