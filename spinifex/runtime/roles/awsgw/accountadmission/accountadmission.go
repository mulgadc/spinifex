// Package accountadmission decides whether a request's account may proceed. The SigV4
// gateway path and the ECR registry bridge both ask it, so account status has one
// enforcement point and one cache.
package accountadmission

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
)

// ActiveTTL bounds how long an account is trusted to still be ACTIVE without
// re-reading it. Only the ACTIVE answer is cached, so suspending or terminating an
// account takes effect within this window rather than being held open by a warm
// entry.
const ActiveTTL = 5 * time.Second

// ErrNotActive means the account exists but is suspended or being torn down.
var ErrNotActive = errors.New("account is not active")

// Admitter reads account status through the IAM service. Every authenticated request
// needs the answer and the account record changes perhaps twice in its lifetime, so
// an uncached read would double the auth path's KV traffic to learn nothing new.
type Admitter struct {
	iam iamdomain.IAMService

	mu     sync.RWMutex
	active map[string]time.Time
}

// New returns an Admitter. A nil iam fails every admission closed.
func New(iam iamdomain.IAMService) *Admitter {
	return &Admitter{iam: iam, active: make(map[string]time.Time)}
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
	if a.activeUntil(accountID, now) {
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

	a.markActive(accountID, now)
	return nil
}

// Forget drops an entry so a status change takes effect immediately on the node that
// made it, rather than after the TTL.
func (a *Admitter) Forget(accountID string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.active, accountID)
}

func (a *Admitter) activeUntil(accountID string, now time.Time) bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	expiry, ok := a.active[accountID]
	return ok && now.Before(expiry)
}

func (a *Admitter) markActive(accountID string, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.active[accountID] = now.Add(ActiveTTL)
}
