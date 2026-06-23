package gateway_ecrauth

import (
	"context"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testRotationParams() rotationParams {
	return rotationParams{rotateAfter: 30 * 24 * time.Hour, retainFor: 24 * time.Hour, retainCount: 2}
}

func TestPlanRotation(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	p := testRotationParams()

	mk := func(kid string, ageHours int) keyMeta {
		return keyMeta{kid: kid, created: now.Add(-time.Duration(ageHours) * time.Hour)}
	}

	t.Run("empty bucket mints", func(t *testing.T) {
		mint, prune := planRotation(nil, now, p)
		assert.True(t, mint)
		assert.Empty(t, prune)
	})

	t.Run("single young key holds", func(t *testing.T) {
		mint, prune := planRotation([]keyMeta{mk("a", 1)}, now, p)
		assert.False(t, mint)
		assert.Empty(t, prune)
	})

	t.Run("single aged key mints, nothing to prune", func(t *testing.T) {
		mint, prune := planRotation([]keyMeta{mk("a", 31*24)}, now, p)
		assert.True(t, mint)
		assert.Empty(t, prune)
	})

	t.Run("previous key within retention is kept", func(t *testing.T) {
		// active 5h old (< retainFor), one previous: still verifiable.
		metas := []keyMeta{mk("new", 5), mk("old", 40*24)}
		mint, prune := planRotation(metas, now, p)
		assert.False(t, mint)
		assert.Empty(t, prune)
	})

	t.Run("previous key past retention is pruned", func(t *testing.T) {
		// active 25h old (>= retainFor): the previous key's tokens have expired.
		metas := []keyMeta{mk("new", 25), mk("old", 40*24)}
		mint, prune := planRotation(metas, now, p)
		assert.False(t, mint)
		assert.Equal(t, []string{"old"}, prune)
	})

	t.Run("retainCount caps previous keys within window", func(t *testing.T) {
		// active 1h old (< retainFor) but three previous keys: keep newest 2.
		metas := []keyMeta{mk("active", 1), mk("p1", 2), mk("p2", 3), mk("p3", 4)}
		mint, prune := planRotation(metas, now, p)
		assert.False(t, mint)
		assert.Equal(t, []string{"p3"}, prune)
	})

	t.Run("mint due prunes stale previous, keeps active", func(t *testing.T) {
		// active is itself due for rotation (31d) and has an ancient previous.
		metas := []keyMeta{mk("active", 31*24), mk("ancient", 60*24)}
		mint, prune := planRotation(metas, now, p)
		assert.True(t, mint)
		assert.Equal(t, []string{"ancient"}, prune) // active retained for next cycle
	})
}

// TestRotateOnce_MintsAndSwaps drives a cycle with rotateAfter=0 so a mint is
// always due: a new active key is minted, installed on the issuer, and accepted
// by the verifier, while the prior key stays verifiable.
func TestRotateOnce_MintsAndSwaps(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	key, verify, err := LoadOrCreateSigningKey(js, testMasterKey, 1)
	require.NoError(t, err)

	issuer := NewIssuer(key, testAudience)
	verifier := NewVerifier(verify, testAudience)
	kv, err := openSigningBucket(js, testMasterKey, 1)
	require.NoError(t, err)

	// Token minted under the original key before rotation.
	oldTok, _, err := issuer.Mint(samplePrincipal())
	require.NoError(t, err)

	r := &Rotator{
		kv: kv, masterKey: testMasterKey, issuer: issuer, verifier: verifier,
		params: rotationParams{rotateAfter: 0, retainFor: time.Hour, retainCount: 2},
		now:    func() time.Time { return time.Now().UTC() },
	}
	r.rotateOnce(time.Now().UTC())

	// A new active key took over.
	assert.NotEqual(t, key.Kid, issuer.ActiveKid(), "issuer should sign with the rotated key")

	// Both old and new tokens verify (previous key retained).
	_, err = verifier.Verify(oldTok)
	require.NoError(t, err, "token from the previous key must still verify")
	newTok, _, err := issuer.Mint(samplePrincipal())
	require.NoError(t, err)
	_, err = verifier.Verify(newTok)
	require.NoError(t, err, "token from the rotated key must verify")

	_, _, metas, err := reloadKeys(kv, testMasterKey)
	require.NoError(t, err)
	assert.Len(t, metas, 2, "previous key retained alongside the new active key")
}

// TestRotateOnce_PrunesExpiredKey seeds a second key, then runs a cycle with
// retainFor=0 (and no mint) so the older key is pruned and dropped from the
// verifier.
func TestRotateOnce_PrunesExpiredKey(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	first, verify, err := LoadOrCreateSigningKey(js, testMasterKey, 1)
	require.NoError(t, err)
	kv, err := openSigningBucket(js, testMasterKey, 1)
	require.NoError(t, err)
	second, err := generateSigningKey(kv, testMasterKey)
	require.NoError(t, err)
	require.NotEqual(t, first.Kid, second.Kid)

	issuer := NewIssuer(first, testAudience)
	verifier := NewVerifier(verify, testAudience)

	r := &Rotator{
		kv: kv, masterKey: testMasterKey, issuer: issuer, verifier: verifier,
		params: rotationParams{rotateAfter: 1000 * time.Hour, retainFor: 0, retainCount: 2},
		now:    func() time.Time { return time.Now().UTC() },
	}
	r.rotateOnce(time.Now().UTC())

	active, _, metas, err := reloadKeys(kv, testMasterKey)
	require.NoError(t, err)
	require.Len(t, metas, 1, "the older key should be pruned")
	assert.Equal(t, second.Kid, active.Kid, "newest key remains active")
	assert.Equal(t, second.Kid, issuer.ActiveKid())
}

// TestRotator_RunRotatesThenStops drives the scheduler loop on a fast interval
// with rotateAfter=0 so it mints on the first tick, then cancels: the active key
// changes and Run returns.
func TestRotator_RunRotatesThenStops(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	key, verify, err := LoadOrCreateSigningKey(js, testMasterKey, 1)
	require.NoError(t, err)
	issuer := NewIssuer(key, testAudience)
	verifier := NewVerifier(verify, testAudience)
	kv, err := openSigningBucket(js, testMasterKey, 1)
	require.NoError(t, err)

	r := &Rotator{
		kv: kv, masterKey: testMasterKey, issuer: issuer, verifier: verifier,
		params:   rotationParams{rotateAfter: 0, retainFor: time.Hour, retainCount: 2},
		interval: time.Millisecond,
		now:      func() time.Time { return time.Now().UTC() },
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { r.Run(ctx); close(stopped) }()

	require.Eventually(t, func() bool { return issuer.ActiveKid() != key.Kid },
		2*time.Second, 5*time.Millisecond, "rotator should mint a new active key")

	cancel()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestIssuerVerifier_ConcurrentSwap exercises the hot-swap under -race: Mint and
// Verify run while SetActiveKey/SetKeys rotate the key set.
func TestIssuerVerifier_ConcurrentSwap(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	k1, v1, err := LoadOrCreateSigningKey(js, testMasterKey, 1)
	require.NoError(t, err)
	kv, err := openSigningBucket(js, testMasterKey, 1)
	require.NoError(t, err)
	k2, err := generateSigningKey(kv, testMasterKey)
	require.NoError(t, err)
	_, v2, _, err := reloadKeys(kv, testMasterKey)
	require.NoError(t, err)

	issuer := NewIssuer(k1, testAudience)
	verifier := NewVerifier(v1, testAudience)

	done := make(chan struct{})
	go func() {
		for range 200 {
			tok, _, mErr := issuer.Mint(samplePrincipal())
			if mErr == nil {
				_, _ = verifier.Verify(tok)
			}
		}
		close(done)
	}()
	for i := range 200 {
		if i%2 == 0 {
			issuer.SetActiveKey(k2)
			verifier.SetKeys(v2)
		} else {
			issuer.SetActiveKey(k1)
			verifier.SetKeys(v1)
		}
	}
	<-done
}
