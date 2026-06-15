package gateway

import (
	"log/slog"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

const (
	failureWindow     = 60 * time.Second // sliding window for auth-failure counting
	maxFailures       = 10               // failures within window before lockout
	initialLockout    = 30 * time.Second // first lockout duration
	backoffMultiplier = 2                // lockout duration multiplier on repeat
	maxLockout        = 5 * time.Minute  // cap on escalating lockout
	gcInterval        = 60 * time.Second // stale-entry eviction interval
)

// ipRecord tracks auth failure state for a single client IP.
type ipRecord struct {
	failures    []time.Time // recent failure timestamps (within window)
	lockedUntil time.Time   // zero = not locked
	lockouts    int         // lockout count for backoff calculation
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

// CheckIP returns "" if the IP may proceed, or ErrorRequestLimitExceeded if locked.
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

// RecordFailure records an auth failure for the IP, locking it out with
// escalating backoff when the threshold is reached.
func (rl *AuthRateLimiter) RecordFailure(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()

	rec, ok := rl.records[ip]
	if !ok {
		rec = &ipRecord{}
		rl.records[ip] = rec
	}

	rec.failures = pruneOldFailures(rec.failures, now)
	rec.failures = append(rec.failures, now)

	if len(rec.failures) >= maxFailures && (rec.lockedUntil.IsZero() || now.After(rec.lockedUntil)) {
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
		rec.failures = nil

		slog.Warn("Rate limit: IP locked out",
			"ip", ip,
			"failures", maxFailures,
			"lockout_duration", lockout,
		)
	}
}

// RecordSuccess clears all failure state for the IP.
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

// cleanup evicts entries whose lockout has expired and all failures are stale.
func (rl *AuthRateLimiter) cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	for ip, rec := range rl.records {
		if !rec.lockedUntil.IsZero() && now.Before(rec.lockedUntil) {
			continue
		}
		rec.failures = pruneOldFailures(rec.failures, now)

		if len(rec.failures) == 0 {
			slog.Debug("Rate limit: GC evicted stale entry", "ip", ip)
			delete(rl.records, ip)
		}
	}
}

// pruneOldFailures returns failures within the sliding window.
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
