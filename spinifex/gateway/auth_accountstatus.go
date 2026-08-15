package gateway

import (
	"log/slog"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

// accountActiveTTL bounds how long an account is trusted to still be ACTIVE
// without re-reading it. Only the ACTIVE answer is cached, so suspending or
// terminating an account takes effect within this window rather than being
// held open by a warm cache entry.
const accountActiveTTL = 5 * time.Second

// accountStatusCache remembers which accounts were ACTIVE and when. Every
// authenticated request needs the answer and the account record changes
// perhaps twice in its lifetime, so an uncached read would double the
// auth path's KV traffic to learn nothing new.
type accountStatusCache struct {
	mu     sync.RWMutex
	active map[string]time.Time
}

func newAccountStatusCache() *accountStatusCache {
	return &accountStatusCache{active: make(map[string]time.Time)}
}

// activeUntil reports whether accountID was seen ACTIVE recently enough.
func (c *accountStatusCache) activeUntil(accountID string, now time.Time) bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	expiry, ok := c.active[accountID]
	return ok && now.Before(expiry)
}

func (c *accountStatusCache) markActive(accountID string, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active[accountID] = now.Add(accountActiveTTL)
}

// forget drops an entry so a status change takes effect immediately on the
// node that made it, rather than after the TTL.
func (c *accountStatusCache) forget(accountID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.active, accountID)
}

// checkAccountActive rejects a request whose account is suspended or being
// torn down. It returns an empty string when the request may proceed.
//
// This is the only enforcement point for account status on the AWS surface:
// until it existed, suspending an account blocked ECR and nothing else.
func (gw *GatewayConfig) checkAccountActive(accountID, clientIP string) string {
	if accountID == "" {
		return ""
	}
	now := time.Now()
	if gw.accountStatus.activeUntil(accountID, now) {
		return ""
	}
	if gw.IAMService == nil {
		slog.Error("Auth: IAM service not initialized for account status check")
		return awserrors.ErrorInternalError
	}

	account, err := gw.IAMService.GetAccount(accountID)
	if err != nil {
		// Fail closed. An account we cannot read is not evidence of an account
		// in good standing, and this gate is what stops a terminating tenant
		// from creating resources the teardown has already walked past.
		slog.Error("Auth: failed to read account for status check", "accountID", accountID, "err", err)
		return awserrors.ErrorInternalError
	}

	if account.Status != handlers_iam.AccountStatusActive {
		slog.Warn("Auth failure: account is not active",
			"accountID", accountID, "status", account.Status, "sourceIP", clientIP)
		return awserrors.ErrorAccessDenied
	}

	gw.accountStatus.markActive(accountID, now)
	return ""
}
