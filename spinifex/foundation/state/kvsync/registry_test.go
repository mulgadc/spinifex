package kvsync_test

import (
	"context"
	"go/build"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
	"github.com/stretchr/testify/assert"
)

// nopStore is a store value for registry tests, which never touch the store.
type nopStore struct{ _ byte }

func (*nopStore) Load(context.Context, string) ([]byte, uint64, error) {
	return nil, 0, kvsync.ErrNotFound
}
func (*nopStore) CompareAndSwap(context.Context, string, []byte, uint64) (uint64, error) {
	return 0, kvsync.ErrUnavailable
}
func (*nopStore) CompareAndDelete(context.Context, string, uint64) error {
	return kvsync.ErrUnavailable
}

var _ kvsync.KVStore = (*nopStore)(nil)

func TestRegistry_PanicsOnACollisionWithinOneStore(t *testing.T) {
	tests := []struct {
		name   string
		first  func(kvsync.KVStore)
		second func(kvsync.KVStore)
	}{
		{"duplicate key",
			func(s kvsync.KVStore) { kvsync.NewLease(s, "vpcd.reconcile", "h", time.Minute) },
			func(s kvsync.KVStore) { kvsync.NewClaim(s, "vpcd.reconcile", "h") }},
		{"key under a registered prefix",
			func(s kvsync.KVStore) { kvsync.NewLeaseGroup(s, "eks.cluster.", "h", time.Minute) },
			func(s kvsync.KVStore) { kvsync.NewLease(s, "eks.cluster.x", "h", time.Minute) }},
		{"prefix over a registered key",
			func(s kvsync.KVStore) { kvsync.NewLease(s, "eks.cluster.x", "h", time.Minute) },
			func(s kvsync.KVStore) { kvsync.NewClaimGroup(s, "eks.", "h") }},
		{"prefix inside a registered prefix",
			func(s kvsync.KVStore) { kvsync.NewLeaseGroup(s, "eks.", "h", time.Minute) },
			func(s kvsync.KVStore) { kvsync.NewClaimGroup(s, "eks.cluster.", "h") }},
		{"prefix containing a registered prefix",
			func(s kvsync.KVStore) { kvsync.NewClaimGroup(s, "eks.cluster.", "h") },
			func(s kvsync.KVStore) { kvsync.NewLeaseGroup(s, "eks.", "h", time.Minute) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			one, two := &nopStore{}, &nopStore{}
			tt.first(one)
			assert.Panics(t, func() { tt.second(one) }, "against one store")
			assert.NotPanics(t, func() { tt.second(two) }, "against another store")
		})
	}
}

func TestRegistry_PanicsOnAKeyJetStreamRefuses(t *testing.T) {
	for _, key := range []string{"", "has space", "star.*", "gt.>", ".lead", "trail.", "two..dots", "_version", "_reserved"} {
		t.Run(key, func(t *testing.T) {
			assert.Panics(t, func() { kvsync.NewClaim(&nopStore{}, key, "h") })
		})
	}
	for _, prefix := range []string{"noDot", "_v.", ".x.", "a b."} {
		t.Run("prefix "+prefix, func(t *testing.T) {
			assert.Panics(t, func() { kvsync.NewLeaseGroup(&nopStore{}, prefix, "h", time.Minute) })
		})
	}
	g := kvsync.NewLeaseGroup(&nopStore{}, "ok.", "h", time.Minute)
	assert.Panics(t, func() { g.Get("bad name") })
	assert.NotPanics(t, func() { g.Get("123456789012/my-cluster/teardown") })
}

func TestPackage_ImportsOnlyTheStandardLibrary(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("read package: %v", err)
	}
	for _, imp := range pkg.Imports {
		// A standard library path has no dot in its first element.
		if first, _, _ := strings.Cut(imp, "/"); strings.Contains(first, ".") {
			t.Errorf("kvsync imports %s; it must depend on nothing but the standard library", imp)
		}
	}
}
