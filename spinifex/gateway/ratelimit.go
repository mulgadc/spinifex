package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

const (
	failureWindow     = 60 * time.Second // sliding window for auth-failure counting
	maxFailures       = 10               // distinct attempts within window before lockout
	initialLockout    = 30 * time.Second // first lockout duration
	backoffMultiplier = 2                // lockout duration multiplier on repeat
	maxLockout        = 5 * time.Minute  // cap on escalating lockout
	gcInterval        = 60 * time.Second // stale-entry eviction interval
)

// anonymousAttempt is the fingerprint for a failure that named no credential —
// a request rejected before its key id was parsed. There is no client identity
// to shield from the lockout, so every occurrence counts.
const anonymousAttempt = ""

// attempt is one distinct failed authentication attempt inside the window.
// Repeating the same attempt refreshes at instead of adding another entry, so
// the count measures how many things were tried rather than how many times.
type attempt struct {
	fingerprint string
	at          time.Time
}

// ipRecord tracks auth failure state for a single client IP.
type ipRecord struct {
	failures    []attempt // distinct recent failed attempts (within window)
	lockedUntil time.Time // zero = not locked
	lockouts    int       // lockout count for backoff calculation
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
// escalating backoff once maxFailures distinct attempts fall inside the window.
// fingerprint identifies the attempt — the key id, the signature it presented,
// whatever the caller had to get right. Repeating one hopeless request is one
// fault however many times it is sent, and must not lock the address out: the
// lockout answers "retry later" to everything from that address, which a client
// whose credential is permanently dead can never act on.
func (rl *AuthRateLimiter) RecordFailure(ip, fingerprint string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()

	rec, ok := rl.records[ip]
	if !ok {
		rec = &ipRecord{}
		rl.records[ip] = rec
	}

	rec.failures = pruneOldFailures(rec.failures, now)
	rec.failures = recordAttempt(rec.failures, fingerprint, now)

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
			"distinct_attempts", maxFailures,
			"lockout_duration", lockout,
		)
	}
}

// failureFingerprint identifies a failed attempt: the reason plus whatever the
// caller had to get right for it. Two failures sharing one are the same fault
// repeated, so only the first of them counts toward a lockout.
func failureFingerprint(reason string, parts ...string) string {
	return reason + "\x00" + strings.Join(parts, "\x00")
}

// tokenDigest reduces a presented session token to a fingerprint component.
// Each distinct token is a distinct guess and must count, but a bearer token has
// no business being held in rate-limiter state.
func tokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
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

// pruneOldFailures returns attempts within the sliding window.
func pruneOldFailures(failures []attempt, now time.Time) []attempt {
	cutoff := now.Add(-failureWindow)
	n := 0
	for _, a := range failures {
		if a.at.After(cutoff) {
			failures[n] = a
			n++
		}
	}
	return failures[:n]
}

// recordAttempt refreshes a fingerprint already in the window, or appends it.
// An empty fingerprint always appends: it means the failure named no credential
// to protect, so repetition is all there is to count. The slice is scanned
// rather than mapped — it holds at most maxFailures entries, because reaching
// that clears it.
func recordAttempt(failures []attempt, fingerprint string, now time.Time) []attempt {
	if fingerprint != anonymousAttempt {
		for i := range failures {
			if failures[i].fingerprint == fingerprint {
				failures[i].at = now
				return failures
			}
		}
	}
	return append(failures, attempt{fingerprint: fingerprint, at: now})
}
