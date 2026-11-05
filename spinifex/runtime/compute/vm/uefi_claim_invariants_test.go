package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
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

// failingStamp is a store record bucket whose stamp fails transiently.
type failingStamp struct{ UEFIStoreRecords }

func (failingStamp) Stamp(context.Context, string, uint64) (*uefistore.UEFIStoreRecord, error) {
	return nil, errors.New("stamp: no responders")
}

// TestS7_UEFIClaimGuardingALocalCopyIsKeptWhenALaunchRollsBack enforces
// ADR-0003:S7: a restart that adopts its claim may hold changes no object
// has, so a launch that fails before reuse is decided must keep the claim.
func TestS7_UEFIClaimGuardingALocalCopyIsKeptWhenALaunchRollsBack(t *testing.T) {
	tests := []struct {
		name  string
		local func(f *uefiFixture, key string, gen uint64)
	}{
		{name: "copy seeded under the claim", local: func(f *uefiFixture, key string, gen uint64) {
			f.writeLocal(t, key, gen, region("guest wrote this"))
		}},
		{name: "unreadable seeded.json", local: func(f *uefiFixture, key string, gen uint64) {
			f.writeLocal(t, key, gen, region("guest wrote this"))
			require.NoError(t, os.WriteFile(filepath.Join(f.instanceDir(), uefiSeededFileName), []byte("{"), 0o600))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newUEFIFixture(t)
			key := keyFor("adopted")
			instance := f.instance(t, key, f.identity)
			tok, err := f.peerClaim("node-a").TryLock(t.Context())
			require.NoError(t, err)
			tt.local(f, key, tok.Generation)
			f.m.deps.UEFIVarStore.Records = failingStamp{f.records}

			err = f.m.Run(t.Context(), instance)

			require.ErrorIs(t, err, ErrMountRetryable)
			holder, held := f.claimHolder(t)
			require.True(t, held, "ADR-0003:S7 (uefi_varstore.go heldLocalCopy): a rolled-back launch released the claim guarding this node's copy; keep it whenever seeded.json names the claim's generation")
			assert.Equal(t, tok, holder)
			_, refused := f.peerClaim("node-b").TryLock(t.Context())
			assert.ErrorAs(t, refused, new(*kvsync.HeldError), "ADR-0003:S7: another node could take the claim while this node's copy held unwritten changes")
		})
	}
}

// A copy seeded under an earlier generation was superseded by another node,
// so a launch that rolls back before it seeds releases the claim it took.
func TestUEFIClaim_RollbackReleasesAClaimNoLocalCopyNeeds(t *testing.T) {
	f := newUEFIFixture(t)
	key := keyFor("stale")
	instance := f.instance(t, key, f.identity)
	earlier, err := f.peerClaim("node-b").TryLock(t.Context())
	require.NoError(t, err)
	require.NoError(t, f.peerClaim("node-b").Unlock(t.Context(), earlier))
	f.writeLocal(t, key, earlier.Generation, region("older run"))
	f.m.deps.UEFIVarStore.Records = failingStamp{f.records}

	require.ErrorIs(t, f.m.Run(t.Context(), instance), ErrMountRetryable)
	_, held := f.claimHolder(t)
	assert.False(t, held)
}
