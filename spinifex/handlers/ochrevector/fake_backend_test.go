package handlers_ochrevector

import (
	"context"
	"errors"
	"sync"
)

// fakeBackend is an in-memory VectorBackend for orchestration tests: no
// Postgres, just enough bookkeeping to assert what Service asked for and to
// inject failures on demand.
type fakeBackend struct {
	mu       sync.Mutex
	accounts map[string]bool
	indexes  map[string]IndexSpec // key: accountID+"/"+indexID

	failEnsureAccount error
	failCreateIndex   error
}

var _ VectorBackend = (*fakeBackend)(nil)

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		accounts: map[string]bool{},
		indexes:  map[string]IndexSpec{},
	}
}

func (f *fakeBackend) Init(_ context.Context) error { return nil }

func (f *fakeBackend) EnsureAccount(_ context.Context, accountID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failEnsureAccount != nil {
		return f.failEnsureAccount
	}
	f.accounts[accountID] = true
	return nil
}

func (f *fakeBackend) CreateIndex(_ context.Context, accountID string, spec IndexSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failCreateIndex != nil {
		return f.failCreateIndex
	}
	f.indexes[accountID+"/"+spec.ID] = spec
	return nil
}

func (f *fakeBackend) DropIndex(_ context.Context, accountID, indexID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.indexes, accountID+"/"+indexID)
	return nil
}

func (f *fakeBackend) hasIndex(accountID, indexID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.indexes[accountID+"/"+indexID]
	return ok
}

func (f *fakeBackend) hasAccount(accountID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accounts[accountID]
}

// errFakeBackend is a stand-in backend failure for rollback/error-path tests.
var errFakeBackend = errors.New("fake backend failure")
