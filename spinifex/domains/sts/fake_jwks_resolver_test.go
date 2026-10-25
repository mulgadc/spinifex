package sts

import (
	"context"
	"slices"
	"sync"
)

// fakeJWKSKey identifies one (account, cluster) entry in a fakeJWKSResolver.
type fakeJWKSKey struct {
	accountID   string
	clusterName string
}

// fakeJWKSEntry is what a fakeJWKSResolver hands back for one fakeJWKSKey.
type fakeJWKSEntry struct {
	raw   []byte
	found bool
	err   error
}

// fakeJWKSResolver is a test-owned ClusterJWKSResolver: a map keyed by
// (accountID, clusterName) that tests seed directly, with no real KV
// involved. Every lookup is recorded so a test can prove which
// account/cluster was actually queried, or that it was never queried at
// all (e.g. an unregistered issuer must short-circuit before any lookup).
type fakeJWKSResolver struct {
	mu      sync.Mutex
	entries map[fakeJWKSKey]fakeJWKSEntry
	calls   []fakeJWKSKey
}

func newFakeJWKSResolver() *fakeJWKSResolver {
	return &fakeJWKSResolver{entries: make(map[fakeJWKSKey]fakeJWKSEntry)}
}

// seed registers raw JWKS bytes as found for (accountID, clusterName).
func (f *fakeJWKSResolver) seed(accountID, clusterName string, raw []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[fakeJWKSKey{accountID, clusterName}] = fakeJWKSEntry{raw: raw, found: true}
}

// seedErr registers a dependency failure to return for (accountID, clusterName).
func (f *fakeJWKSResolver) seedErr(accountID, clusterName string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[fakeJWKSKey{accountID, clusterName}] = fakeJWKSEntry{err: err}
}

// LookupClusterJWKS implements ClusterJWKSResolver. An unseeded key reads as
// (nil, false, nil) — not found — the same zero value a real resolver uses.
func (f *fakeJWKSResolver) LookupClusterJWKS(_ context.Context, accountID, clusterName string) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := fakeJWKSKey{accountID, clusterName}
	f.calls = append(f.calls, key)
	e := f.entries[key]
	return e.raw, e.found, e.err
}

// lookups returns every (accountID, clusterName) pair LookupClusterJWKS was
// called with, in call order.
func (f *fakeJWKSResolver) lookups() []fakeJWKSKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeJWKSKey(nil), f.calls...)
}

// calledWith reports whether LookupClusterJWKS was ever called for
// (accountID, clusterName).
func (f *fakeJWKSResolver) calledWith(accountID, clusterName string) bool {
	return slices.Contains(f.lookups(), (fakeJWKSKey{accountID, clusterName}))
}
