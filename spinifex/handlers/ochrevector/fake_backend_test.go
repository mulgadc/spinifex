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

	docs                 map[string]map[string][]VectorRow // key: accountID+"/"+indexID -> sourceKey -> rows
	replaceDocumentCalls map[string]int                    // key: accountID+"/"+indexID+"/"+sourceKey -> call count

	failEnsureAccount      error
	failCreateIndex        error
	failReplaceDocument    error
	failReplaceDocumentFor map[string]error // key: accountID+"/"+indexID+"/"+sourceKey -> error, for per-key failure injection
}

var _ VectorBackend = (*fakeBackend)(nil)

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		accounts:             map[string]bool{},
		indexes:              map[string]IndexSpec{},
		docs:                 map[string]map[string][]VectorRow{},
		replaceDocumentCalls: map[string]int{},
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

// ReplaceDocument records rows under accountID+"/"+indexID+"/"+sourceKey,
// overwriting whatever was there before -- mirroring pgxBackend's
// delete-then-reinsert semantics without a real transaction, since a Go map
// assignment is already all-or-nothing from the caller's perspective.
func (f *fakeBackend) ReplaceDocument(_ context.Context, accountID, indexID, sourceKey string, rows []VectorRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	docKey := accountID + "/" + indexID
	callKey := docKey + "/" + sourceKey
	f.replaceDocumentCalls[callKey]++

	if f.failReplaceDocument != nil {
		return f.failReplaceDocument
	}
	if err, ok := f.failReplaceDocumentFor[callKey]; ok {
		return err
	}

	if f.docs[docKey] == nil {
		f.docs[docKey] = map[string][]VectorRow{}
	}
	f.docs[docKey][sourceKey] = append([]VectorRow(nil), rows...)
	return nil
}

// documentRows returns the current row set stored for sourceKey, or nil if
// ReplaceDocument was never called for it.
func (f *fakeBackend) documentRows(accountID, indexID, sourceKey string) []VectorRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.docs[accountID+"/"+indexID][sourceKey]
}

// replaceDocumentCallCount reports how many times ReplaceDocument was called
// for sourceKey, so a re-run test can assert "called again" without the row
// set having doubled.
func (f *fakeBackend) replaceDocumentCallCount(accountID, indexID, sourceKey string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.replaceDocumentCalls[accountID+"/"+indexID+"/"+sourceKey]
}

// errFakeBackend is a stand-in backend failure for rollback/error-path tests.
var errFakeBackend = errors.New("fake backend failure")
