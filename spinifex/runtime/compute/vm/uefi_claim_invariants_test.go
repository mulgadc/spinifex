package vm

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// holdOnPeer has node-b claim the instance's store and stamp its record, as
// the node running the instance has, and returns node-b's token.
func holdOnPeer(t *testing.T, f *uefiFixture) (*VM, uint64) {
	t.Helper()
	key := keyFor("held")
	f.putObject(t, key, gzipped(t, region("node-b booted this")))
	instance := f.instance(t, key, f.identity)
	tok, err := f.peerClaim("node-b").TryLock(t.Context())
	require.NoError(t, err)
	_, err = f.records.Stamp(t.Context(), testUEFIInstance, tok.Generation)
	require.NoError(t, err)
	return instance, tok.Generation
}

// TestS7_UEFIClaimIsNeverTakenOverWhileItsHolderHeartbeats enforces ADR-0003:S7
// for the store claim: a timeout or a missed heartbeat is never a decision, so a
// start elsewhere must not take a claim whose holder heartbeated under
// clusterv1.LivenessWindow ago, or whose heartbeat cannot be read. It is refused
// as retryable, and the claim, the record and the volumes are untouched.
func TestS7_UEFIClaimIsNeverTakenOverWhileItsHolderHeartbeats(t *testing.T) {
	tests := []struct {
		name   string
		age    time.Duration
		err    error
		reason string
	}{
		{name: "heartbeat just written", age: 0, reason: "holder_live"},
		{name: "heartbeat 30s old", age: 30 * time.Second, reason: "holder_live"},
		{name: "heartbeat 59s old", age: 59 * time.Second, reason: "holder_live"},
		{name: "heartbeat unreadable", err: errors.New("cluster-state bucket unreachable"), reason: "liveness_unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newUEFIFixture(t)
			instance, gen := holdOnPeer(t, f)
			f.liveness.heartbeat("node-b", tt.age)
			f.liveness.err = tt.err
			_, rev := f.record(t)
			before := uefiCounter(t, "mulga.vm.uefi_store.claim_refusals", "reason", tt.reason)

			err := f.m.Run(t.Context(), instance)

			require.ErrorIs(t, err, ErrUEFIClaimHeld,
				"ADR-0003:S7 (uefi_varstore.go acquireUEFIClaim): a start must be refused while the claim's holder may be live; take over only once clusterv1.LiveAt is false")
			require.ErrorIs(t, err, ErrMountRetryable, "the refusal clears once the holder stops or releases, so restore retries it")
			holder, held := f.claimHolder(t)
			require.True(t, held, "ADR-0003:S7: the holder's claim was removed while it heartbeated")
			assert.Equal(t, "node-b", holder.Holder, "ADR-0003:S7: the claim was taken over while its holder heartbeated")
			assert.Equal(t, gen, holder.Generation)
			_, after := f.record(t)
			assert.Equal(t, rev, after, "ADR-0003:S7: a refused start must not stamp the store record")
			assert.Empty(t, f.mounter.mounted)
			assert.Equal(t, before+1, uefiCounter(t, "mulga.vm.uefi_store.claim_refusals", "reason", tt.reason))
		})
	}
}

// Once the holder's heartbeat is LivenessWindow old, or gone, its node is lost,
// and the start takes the claim over, stamps the record above the old holder
// and counts it, so the old holder's pointer moves are fenced from then on.
func TestS7_UEFIClaimIsTakenOverOnceItsHolderStopsHeartbeating(t *testing.T) {
	tests := []struct {
		name  string
		age   time.Duration
		noKey bool
	}{
		{name: "heartbeat 60s old", age: 60 * time.Second},
		{name: "heartbeat 10m old", age: 10 * time.Minute},
		{name: "no heartbeat", noKey: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newUEFIFixture(t)
			instance, gen := holdOnPeer(t, f)
			if !tt.noKey {
				f.liveness.heartbeat("node-b", tt.age)
			}
			before := uefiCounter(t, "mulga.vm.uefi_store.claim_takeovers", "", "")

			held, err := f.prepare(t, instance)

			require.NoError(t, err)
			holder, ok := f.claimHolder(t)
			require.True(t, ok)
			assert.Equal(t, "node-a", holder.Holder)
			assert.Greater(t, holder.Generation, gen)
			assert.Equal(t, held.token, holder)
			rec, _ := f.record(t)
			assert.Equal(t, holder.Generation, rec.Generation, "the new owner stamps the record before it reads the pointer")
			assert.Equal(t, region("node-b booted this"), f.localVars(t))
			assert.Equal(t, before+1, uefiCounter(t, "mulga.vm.uefi_store.claim_takeovers", "", ""))
		})
	}
}
