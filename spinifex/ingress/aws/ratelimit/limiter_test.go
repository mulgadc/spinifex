//test:in-package — these tests drive the record table and backoff state the
// limiter keeps unexported.

package ratelimit

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fmt"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// distinctFingerprint returns a fingerprint no other attempt shares, which is
// the shape of credential guessing: the lockout counts distinct attempts, so a
// test driving it to the threshold must vary them.
func distinctFingerprint() string {
	return Fingerprint("test", strconv.Itoa(int(fingerprintSeq.Add(1))))
}

var fingerprintSeq atomic.Int64

func TestCheckIP_AllowsUnknownIP(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	if errCode := rl.CheckIP("10.0.0.1"); errCode != "" {
		t.Fatalf("expected empty error for unknown IP, got %q", errCode)
	}
}

func TestRecordFailure_BelowThreshold(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	ip := "10.0.0.2"
	for range MaxFailures - 1 {
		rl.RecordFailure(ip, distinctFingerprint())
	}

	if errCode := rl.CheckIP(ip); errCode != "" {
		t.Fatalf("expected IP to be allowed after %d failures, got %q", MaxFailures-1, errCode)
	}
}

func TestRecordFailure_AtThreshold(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	ip := "10.0.0.3"
	for range MaxFailures {
		rl.RecordFailure(ip, distinctFingerprint())
	}

	if errCode := rl.CheckIP(ip); errCode != awserrors.ErrorRequestLimitExceeded {
		t.Fatalf("expected %s after %d failures, got %q", awserrors.ErrorRequestLimitExceeded, MaxFailures, errCode)
	}
}

func TestCheckIP_RejectsLockedIP(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	ip := "10.0.0.4"
	for range MaxFailures {
		rl.RecordFailure(ip, distinctFingerprint())
	}

	if errCode := rl.CheckIP(ip); errCode != awserrors.ErrorRequestLimitExceeded {
		t.Fatalf("expected locked IP to be rejected, got %q", errCode)
	}

	if errCode := rl.CheckIP(ip); errCode != awserrors.ErrorRequestLimitExceeded {
		t.Fatalf("expected locked IP to still be rejected, got %q", errCode)
	}
}

func TestRecordSuccess_ClearsState(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	ip := "10.0.0.5"
	// Accumulate failures but stay below threshold.
	for range MaxFailures - 1 {
		rl.RecordFailure(ip, distinctFingerprint())
	}

	rl.RecordSuccess(ip)

	// After success, all state should be cleared — can accumulate failures again from 0.
	for range MaxFailures - 1 {
		rl.RecordFailure(ip, distinctFingerprint())
	}

	if errCode := rl.CheckIP(ip); errCode != "" {
		t.Fatalf("expected IP to be allowed after success reset, got %q", errCode)
	}
}

func TestRecordSuccess_ClearsLockout(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	ip := "10.0.0.6"
	for range MaxFailures {
		rl.RecordFailure(ip, distinctFingerprint())
	}

	if errCode := rl.CheckIP(ip); errCode == "" {
		t.Fatal("expected IP to be locked")
	}

	rl.RecordSuccess(ip)

	if errCode := rl.CheckIP(ip); errCode != "" {
		t.Fatalf("expected IP to be allowed after success, got %q", errCode)
	}
}

func TestEscalatingBackoff(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	ip := "10.0.0.7"

	// First lockout: 30s
	for range MaxFailures {
		rl.RecordFailure(ip, distinctFingerprint())
	}

	rl.mu.Lock()
	rec := rl.records[ip]
	firstLockout := time.Until(rec.lockedUntil)
	rl.mu.Unlock()

	if firstLockout > initialLockout+time.Second || firstLockout < initialLockout-time.Second {
		t.Fatalf("expected first lockout ~%v, got %v", initialLockout, firstLockout)
	}

	// Simulate lockout expiry and trigger second lockout.
	rl.mu.Lock()
	rec.lockedUntil = time.Now().Add(-time.Second) // expired
	rec.failures = nil                             // reset failures for next round
	rl.mu.Unlock()

	for range MaxFailures {
		rl.RecordFailure(ip, distinctFingerprint())
	}

	rl.mu.Lock()
	secondLockout := time.Until(rec.lockedUntil)
	rl.mu.Unlock()

	expectedSecond := initialLockout * backoffMultiplier
	if secondLockout > expectedSecond+time.Second || secondLockout < expectedSecond-time.Second {
		t.Fatalf("expected second lockout ~%v, got %v", expectedSecond, secondLockout)
	}

	// Third lockout: 120s
	rl.mu.Lock()
	rec.lockedUntil = time.Now().Add(-time.Second)
	rec.failures = nil
	rl.mu.Unlock()

	for range MaxFailures {
		rl.RecordFailure(ip, distinctFingerprint())
	}

	rl.mu.Lock()
	thirdLockout := time.Until(rec.lockedUntil)
	rl.mu.Unlock()

	expectedThird := initialLockout * backoffMultiplier * backoffMultiplier
	if thirdLockout > expectedThird+time.Second || thirdLockout < expectedThird-time.Second {
		t.Fatalf("expected third lockout ~%v, got %v", expectedThird, thirdLockout)
	}

	// Fourth lockout: 30s * 2^3 = 240s = 4m.
	rl.mu.Lock()
	rec.lockedUntil = time.Now().Add(-time.Second)
	rec.failures = nil
	rl.mu.Unlock()

	for range MaxFailures {
		rl.RecordFailure(ip, distinctFingerprint())
	}

	rl.mu.Lock()
	fourthLockout := time.Until(rec.lockedUntil)
	rl.mu.Unlock()

	expectedFourth := initialLockout * backoffMultiplier * backoffMultiplier * backoffMultiplier
	if fourthLockout > expectedFourth+time.Second || fourthLockout < expectedFourth-time.Second {
		t.Fatalf("expected fourth lockout ~%v, got %v", expectedFourth, fourthLockout)
	}

	// Fifth lockout: 30s * 2^4 = 480s, capped at maxLockout (300s).
	rl.mu.Lock()
	rec.lockedUntil = time.Now().Add(-time.Second)
	rec.failures = nil
	rl.mu.Unlock()

	for range MaxFailures {
		rl.RecordFailure(ip, distinctFingerprint())
	}

	rl.mu.Lock()
	fifthLockout := time.Until(rec.lockedUntil)
	rl.mu.Unlock()

	if fifthLockout > maxLockout+time.Second || fifthLockout < maxLockout-time.Second {
		t.Fatalf("expected fifth lockout to cap at ~%v, got %v", maxLockout, fifthLockout)
	}
}

func TestFailureWindowSliding(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	ip := "10.0.0.8"

	// Inject failures that are outside the sliding window.
	rl.mu.Lock()
	rec := &ipRecord{}
	oldTime := time.Now().Add(-failureWindow - time.Second)
	for range MaxFailures - 1 {
		rec.failures = append(rec.failures, attempt{fingerprint: distinctFingerprint(), at: oldTime})
	}
	rl.records[ip] = rec
	rl.mu.Unlock()

	// Add one recent failure — total "recent" failures should be just 1.
	rl.RecordFailure(ip, distinctFingerprint())

	if errCode := rl.CheckIP(ip); errCode != "" {
		t.Fatalf("expected IP to be allowed (old failures expired), got %q", errCode)
	}
}

func TestCleanup_EvictsStaleEntries(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	ip := "10.0.0.9"

	// Insert a stale entry: lockout expired and all failures old.
	rl.mu.Lock()
	rl.records[ip] = &ipRecord{
		failures:    []attempt{{fingerprint: "stale", at: time.Now().Add(-failureWindow - time.Second)}},
		lockedUntil: time.Now().Add(-time.Second),
		lockouts:    1,
	}
	rl.mu.Unlock()

	rl.cleanup()

	rl.mu.Lock()
	_, exists := rl.records[ip]
	rl.mu.Unlock()

	if exists {
		t.Fatal("expected stale entry to be evicted by cleanup")
	}
}

func TestCleanup_KeepsActiveEntries(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	ip := "10.0.0.10"

	// Insert an entry that's still locked.
	rl.mu.Lock()
	rl.records[ip] = &ipRecord{
		failures:    []attempt{{fingerprint: "recent", at: time.Now()}},
		lockedUntil: time.Now().Add(30 * time.Second),
		lockouts:    1,
	}
	rl.mu.Unlock()

	rl.cleanup()

	rl.mu.Lock()
	_, exists := rl.records[ip]
	rl.mu.Unlock()

	if !exists {
		t.Fatal("expected active entry to be kept by cleanup")
	}
}

func TestConcurrentAccess(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	var wg sync.WaitGroup
	ips := []string{"10.0.0.20", "10.0.0.21", "10.0.0.22"}

	for _, ip := range ips {
		for range 20 {
			wg.Go(func() {
				rl.CheckIP(ip)
				rl.RecordFailure(ip, distinctFingerprint())
				rl.RecordSuccess(ip)
				rl.CheckIP(ip)
			})
		}
	}

	wg.Wait()
}

// The case this was written for: four guests holding deleted session
// credentials sent 53,392 rejected requests and locked their addresses out for
// five days. One credential presented 14,000 times is one fault, and the
// lockout tells a client to retry later when it can never succeed.
func TestRecordFailureIgnoresARepeatedAttempt(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	const ip = "10.15.8.11"
	stale := Fingerprint("session-not-found", "ASIASTALEKEY00000000")
	for range MaxFailures * 5 {
		rl.RecordFailure(ip, stale)
	}

	assert.Empty(t, rl.CheckIP(ip), "one attempt repeated must never lock the address out")

	rl.mu.RLock()
	defer rl.mu.RUnlock()
	assert.Len(t, rl.records[ip].failures, 1, "the window holds one entry per distinct attempt")
}

// Guessing has to stay rate limited: whatever the attacker varies is what the
// fingerprint carries, so each guess is a fresh attempt.
func TestRecordFailureLocksOutDistinctAttempts(t *testing.T) {
	tests := []struct {
		name string
		fp   func(i int) string
	}{
		{"key ids", func(i int) string {
			return Fingerprint("akid-not-found", fmt.Sprintf("AKIAGUESS%d", i))
		}},
		{"signatures for one key", func(i int) string {
			return Fingerprint("signature", "AKIAVALIDKEY00000000", fmt.Sprintf("sig-%d", i))
		}},
		{"session tokens for one key", func(i int) string {
			return Fingerprint("session-token-mismatch", "ASIAVALIDKEY00000000", TokenDigest(fmt.Sprintf("token-%d", i)))
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rl := NewAuthRateLimiter()
			defer rl.Stop()

			ip := "10.15.9." + strconv.Itoa(len(tc.name))
			for i := range MaxFailures {
				rl.RecordFailure(ip, tc.fp(i))
			}

			assert.Equal(t, awserrors.ErrorRequestLimitExceeded, rl.CheckIP(ip))
		})
	}
}

// A repeat refreshes the entry it matches. Without that a client retrying
// steadily would drop out of the window and re-enter it, which is harmless here
// but would make the count depend on retry timing rather than on what was tried.
func TestRecordFailureRefreshesARepeatedAttempt(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	const ip = "10.15.8.12"
	fp := Fingerprint("session-not-found", "ASIASTALEKEY00000000")
	rl.RecordFailure(ip, fp)

	rl.mu.Lock()
	rl.records[ip].failures[0].at = time.Now().Add(-failureWindow / 2)
	rl.mu.Unlock()

	rl.RecordFailure(ip, fp)

	rl.mu.RLock()
	defer rl.mu.RUnlock()
	require.Len(t, rl.records[ip].failures, 1)
	assert.WithinDuration(t, time.Now(), rl.records[ip].failures[0].at, time.Second)
}
