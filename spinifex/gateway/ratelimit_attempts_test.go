package gateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The case this was written for: four guests holding deleted session
// credentials sent 53,392 rejected requests and locked their addresses out for
// five days. One credential presented 14,000 times is one fault, and the
// lockout tells a client to retry later when it can never succeed.
func TestRecordFailureIgnoresARepeatedAttempt(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	const ip = "10.15.8.11"
	stale := failureFingerprint("session-not-found", "ASIASTALEKEY00000000")
	for range maxFailures * 5 {
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
			return failureFingerprint("akid-not-found", fmt.Sprintf("AKIAGUESS%d", i))
		}},
		{"signatures for one key", func(i int) string {
			return failureFingerprint("signature", "AKIAVALIDKEY00000000", fmt.Sprintf("sig-%d", i))
		}},
		{"session tokens for one key", func(i int) string {
			return failureFingerprint("session-token-mismatch", "ASIAVALIDKEY00000000", tokenDigest(fmt.Sprintf("token-%d", i)))
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rl := NewAuthRateLimiter()
			defer rl.Stop()

			ip := "10.15.9." + strconv.Itoa(len(tc.name))
			for i := range maxFailures {
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
	fp := failureFingerprint("session-not-found", "ASIASTALEKEY00000000")
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

// End to end: a client whose credential will never resolve keeps getting the
// verdict that says so, and never the 503 that tells it to retry.
func TestStaleCredentialNeverLocksTheAddressOut(t *testing.T) {
	handler, _ := auditRouter(t, map[string]*handlers_iam.AccessKey{})

	for range maxFailures * 3 {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, signedRequest("10.15.8.14:54321"))
		require.Equal(t, http.StatusForbidden, w.Code, "a dead credential is a client fault, not throttling")
	}
}
