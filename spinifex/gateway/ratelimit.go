package gateway

import (
	"log/slog"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

const (
	// failureWindow is the sliding window for counting auth failures.
	failureWindow = 60 * time.Second

	// maxFailures is the number of consecutive failures within the window before lockout.
	maxFailures = 10

	// initialLockout is the first lockout duration after hitting the threshold.
	initialLockout = 30 * time.Second

	// backoffMultiplier scales the lockout duration on repeated lockouts.
	backoffMultiplier = 2

	// maxLockout caps the escalating lockout duration.
	maxLockout = 5 * time.Minute

	// gcInterval is how often stale entries are evicted.
	gcInterval = 60 * time.Second
)

// ipRecord tracks auth failure state for a single client IP.
type ipRecord struct {
	failures    []time.Time // timestamps of recent failures (within window)
	lockedUntil time.Time   // zero value = not locked
	lockouts    int         // number of times this IP has been locked out (for backoff)
}

// AuthRateLimiter tracks per-IP authentication failure rates and enforces
// escalating lockouts after repeated failures.
type AuthRateLimiter struct {
	mu      sync.RWMutex
	records map[string]*ipRecord
	stop    chan struct{}
	done    chan struct{}
}

// NewAuthRateLimiter creates and starts an AuthRateLimiter with background GC.
// Call Stop to shut down the GC goroutine.
func NewAuthRateLimiter() *AuthRateLimiter {
	rl := &AuthRateLimiter{
		records: make(map[string]*ipRecord),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go rl.gcLoop()
	return rl
}

// Stop cancels the background GC goroutine and waits for it to exit.
func (rl *AuthRateLimiter) Stop() {
	select {
	case <-rl.stop:
		// Already stopped.
	default:
		close(rl.stop)
	}
	<-rl.done
}

// CheckIP returns an empty string if the IP is allowed to proceed, or an error
// code matching ErrorRequestLimitExceeded if the IP is currently locked out.
func (rl *AuthRateLimiter) CheckIP(ip string) string {
	rl.mu.RLock()
	defer rl.mu.RUnlock()

	rec, ok := rl.records[ip]
	if !ok {
		return ""
	}

	if !rec.lockedUntil.IsZero() && time.Now().Before(rec.lockedUntil) {
		slog.Debug("Rate limit: rejecting locked IP", "ip", ip, "locked_until", rec.lockedUntil)
		return awserrors.ErrorRequestLimitExceeded
	}

	return ""
}

// RecordFailure records an authentication failure for the given IP. If the
// failure count within the sliding window reaches the threshold, the IP is
// locked out with escalating backoff.
func (rl *AuthRateLimiter) RecordFailure(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()

	rec, ok := rl.records[ip]
	if !ok {
		rec = &ipRecord{}
		rl.records[ip] = rec
	}

	// Prune failures outside the sliding window.
	rec.failures = pruneOldFailures(rec.failures, now)

	rec.failures = append(rec.failures, now)

	if len(rec.failures) >= maxFailures && (rec.lockedUntil.IsZero() || now.After(rec.lockedUntil)) {
		// Calculate lockout duration with escalating backoff.
		lockout := initialLockout
		for range rec.lockouts {
			lockout *= time.Duration(backoffMultiplier)
			if lockout >= maxLockout {
				lockout = maxLockout
				break
			}
		}
		rec.lockedUntil = now.Add(lockout)
		rec.lockouts++
		rec.failures = nil // Reset for the next window after lockout.

		slog.Warn("Rate limit: IP locked out",
			"ip", ip,
			"failures", maxFailures,
			"lockout_duration", lockout,
		)
	}
}

// RecordSuccess clears all failure state for the given IP, immediately
// restoring access for legitimate clients.
func (rl *AuthRateLimiter) RecordSuccess(ip string) {
	rl.mu.RLock()
	_, ok := rl.records[ip]
	rl.mu.RUnlock()
	if !ok {
		return
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()
	if _, ok := rl.records[ip]; ok {
		slog.Info("Rate limit: IP lockout cleared on success", "ip", ip)
		delete(rl.records, ip)
	}
}

// gcLoop runs cleanup on a fixed interval until Stop is called.
func (rl *AuthRateLimiter) gcLoop() {
	defer close(rl.done)
	ticker := time.NewTicker(gcInterval)
	defer ticker.Stop()

	for {
		select {
		case <-rl.stop:
			return
		case <-ticker.C:
			rl.cleanup()
		}
	}
}

// cleanup evicts stale entries whose lockout has expired and whose failures
// are all outside the sliding window.
func (rl *AuthRateLimiter) cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	for ip, rec := range rl.records {
		// Skip if still locked.
		if !rec.lockedUntil.IsZero() && now.Before(rec.lockedUntil) {
			continue
		}

		// Prune old failures.
		rec.failures = pruneOldFailures(rec.failures, now)

		if len(rec.failures) == 0 {
			slog.Debug("Rate limit: GC evicted stale entry", "ip", ip)
			delete(rl.records, ip)
		}
	}
}

// pruneOldFailures returns only the failures within the sliding window.
func pruneOldFailures(failures []time.Time, now time.Time) []time.Time {
	cutoff := now.Add(-failureWindow)
	n := 0
	for _, t := range failures {
		if t.After(cutoff) {
			failures[n] = t
			n++
		}
	}
	return failures[:n]
}
