package kvsync

import (
	"fmt"
	"strings"
	"sync"
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

// registerKey panics unless key is valid and neither registered nor under a
// registered prefix of store.
func registerKey(store KVStore, key string) {
	if err := validKey(key); err != nil {
		panic(err) //nolint:forbidigo // Constructing a lock wrongly is a wiring error that must fail at startup, not at the first election.
	}
	set := keySetFor(store)
	defer registries.mu.Unlock()
	if _, dup := set.keys[key]; dup {
		panic(fmt.Sprintf("kvsync: key %q constructed twice against one store", key)) //nolint:forbidigo // Constructing a lock wrongly is a wiring error that must fail at startup, not at the first election.
	}
	for p := range set.prefixes {
		if strings.HasPrefix(key, p) {
			panic(fmt.Sprintf("kvsync: key %q is under registered prefix %q", key, p)) //nolint:forbidigo // Constructing a lock wrongly is a wiring error that must fail at startup, not at the first election.
		}
	}
	set.keys[key] = struct{}{}
}

// registerPrefix panics unless prefix is valid, no registered prefix contains
// it or is contained by it, and no registered key falls under it.
func registerPrefix(store KVStore, prefix string) {
	if err := validPrefix(prefix); err != nil {
		panic(err) //nolint:forbidigo // Constructing a lock wrongly is a wiring error that must fail at startup, not at the first election.
	}
	set := keySetFor(store)
	defer registries.mu.Unlock()
	for p := range set.prefixes {
		if strings.HasPrefix(prefix, p) || strings.HasPrefix(p, prefix) {
			panic(fmt.Sprintf("kvsync: prefix %q overlaps registered prefix %q", prefix, p)) //nolint:forbidigo // Constructing a lock wrongly is a wiring error that must fail at startup, not at the first election.
		}
	}
	for k := range set.keys {
		if strings.HasPrefix(k, prefix) {
			panic(fmt.Sprintf("kvsync: prefix %q covers registered key %q", prefix, k)) //nolint:forbidigo // Constructing a lock wrongly is a wiring error that must fail at startup, not at the first election.
		}
	}
	set.prefixes[prefix] = struct{}{}
}

// keySetFor returns store's key set with registries.mu held; the caller unlocks.
func keySetFor(store KVStore) *keySet {
	if store == nil {
		panic("kvsync: nil store") //nolint:forbidigo // Constructing a lock wrongly is a wiring error that must fail at startup, not at the first election.
	}
	registries.mu.Lock()
	set, ok := registries.m[store]
	if !ok {
		set = &keySet{keys: map[string]struct{}{}, prefixes: map[string]struct{}{}}
		registries.m[store] = set
	}
	return set
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
