// Package accountadmission decides whether a request's account may proceed. The SigV4
// gateway path and the ECR registry bridge both ask it, so account status has one
// enforcement point and one cache.
package accountadmission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
	"github.com/nats-io/nats.go/jetstream"
)

// ActiveTTL is the backstop on how long an account is trusted to still be ACTIVE
// without re-reading it. Only the ACTIVE answer is cached, and a running watcher
// evicts an entry the moment the account record changes, so the TTL only matters if
// an update is somehow missed.
const ActiveTTL = 5 * time.Second

// ErrNotActive means the account exists but is suspended or being torn down.
var ErrNotActive = credential.ErrAccountNotActive

// activeEntry is a cached ACTIVE answer and the account generation it was read at.
type activeEntry struct {
	until      time.Time
	generation uint64
}

// Admitter reads account status through the IAM service. Every authenticated request
// needs the answer and the account record changes perhaps twice in its lifetime, so
// an uncached read would double the auth path's KV traffic to learn nothing new.
//
// The cache is only trusted while Watch is running: without a watcher a status change
// made by another process could be held open for the whole TTL, so every request reads.
type Admitter struct {
	iam iamdomain.IAMService

	watching atomic.Bool

	mu     sync.RWMutex
	active map[string]activeEntry
	// latest is the newest generation the watcher has seen per account, so a read that
	// raced an update cannot be cached after the update evicted its predecessor.
	latest map[string]uint64
}

// New returns an Admitter. A nil iam fails every admission closed.
func New(iam iamdomain.IAMService) *Admitter {
	return &Admitter{iam: iam, active: make(map[string]activeEntry), latest: make(map[string]uint64)}
}

// Watch follows the accounts bucket until ctx is cancelled, evicting an account's cached
// entry when its record changes or is deleted. It returns once the initial state has
// been replayed, so the cache is already trusted; the returned channel closes when the
// watcher has stopped and the cache is bypassed again.
func (a *Admitter) Watch(ctx context.Context, accounts jetstream.KeyValue) (<-chan struct{}, error) {
	w, err := accounts.WatchAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("watch accounts: %w", err)
	}

	a.mu.Lock()
	clear(a.active)
	clear(a.latest)
	a.mu.Unlock()

	replayed := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer a.watching.Store(false)
		defer func() { _ = w.Stop() }()
		var once sync.Once
		for entry := range w.Updates() {
			if entry == nil {
				once.Do(func() {
					a.watching.Store(true)
					close(replayed)
				})
				continue
			}
			a.observe(entry)
		}
		once.Do(func() { close(replayed) })
	}()

	select {
	case <-replayed:
	case <-ctx.Done():
		<-done
		return nil, ctx.Err()
	}
	if !a.watching.Load() {
		return done, errors.New("account watcher stopped before replay finished")
	}
	return done, nil
}

// observe applies one watched record to the cache.
func (a *Admitter) observe(entry jetstream.KeyValueEntry) {
	accountID := entry.Key()
	a.mu.Lock()
	defer a.mu.Unlock()

	if entry.Operation() != jetstream.KeyValuePut {
		// The account is gone and its ID is never reissued, so nothing read before
		// the delete may be cached after it.
		a.latest[accountID] = math.MaxUint64
		delete(a.active, accountID)
		return
	}

	var account iamdomain.Account
	if err := json.Unmarshal(entry.Value(), &account); err != nil {
		slog.Warn("Auth: unreadable account record from watcher, evicting", "accountID", accountID, "err", err)
		delete(a.active, accountID)
		return
	}
	a.latest[accountID] = account.Generation
	if cached, ok := a.active[accountID]; ok && (account.Generation > cached.generation || account.Status != iamdomain.AccountStatusActive) {
		delete(a.active, accountID)
	}
}

// Admit returns nil when the account may proceed, ErrNotActive when it may not, and
// any other error when its status could not be read. An unreadable account is not
// evidence of one in good standing, so callers must fail closed on every error. An
// empty accountID is admitted: the caller has no account to gate. logIP is for log
// lines only.
func (a *Admitter) Admit(accountID, logIP string) error {
	if accountID == "" {
		return nil
	}
	now := time.Now()
	if a != nil && a.watching.Load() && a.activeUntil(accountID, now) {
		return nil
	}
	if a == nil || a.iam == nil {
		slog.Error("Auth: IAM service not initialized for account status check")
		return errors.New("IAM service not initialized")
	}

	account, err := a.iam.GetAccount(accountID)
	if err != nil {
		// This gate is what stops a terminating tenant from creating resources the
		// teardown has already walked past.
		slog.Error("Auth: failed to read account for status check", "accountID", accountID, "err", err)
		return fmt.Errorf("read account: %w", err)
	}

	if account.Status != iamdomain.AccountStatusActive {
		slog.Warn("Auth failure: account is not active",
			"accountID", accountID, "status", account.Status, "sourceIP", logIP)
		return ErrNotActive
	}

	if a.watching.Load() {
		a.markActive(accountID, account.Generation, now)
	}
	return nil
}

func (a *Admitter) activeUntil(accountID string, now time.Time) bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	entry, ok := a.active[accountID]
	return ok && now.Before(entry.until)
}

// markActive caches an ACTIVE read unless the watcher has already seen a newer record.
func (a *Admitter) markActive(accountID string, generation uint64, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if latest, ok := a.latest[accountID]; ok && latest > generation {
		return
	}
	a.active[accountID] = activeEntry{until: now.Add(ActiveTTL), generation: generation}
}

// Active reports whether the account may proceed. A suspended, terminating or deleted
// account is (false, nil); only an unreadable status is an error.
func (a *Admitter) Active(accountID, logIP string) (bool, error) {
	err := a.Admit(accountID, logIP)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrNotActive), awserrors.IsErrorCode(err, awserrors.ErrorIAMNoSuchEntity):
		return false, nil
	default:
		return false, err
	}
}
