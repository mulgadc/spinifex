package viperblockd

import (
	"context"
	"fmt"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestLeases binds a lease store for owner against natsURL's JetStream.
func newTestLeases(t *testing.T, natsURL, owner string) *volumeLeases {
	t.Helper()

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	leases, err := newVolumeLeases(t.Context(), nc, owner)
	require.NoError(t, err)
	return leases
}

// TestVolumeLease_SecondNodeIsRefused is the exclusion itself. Two viperblock
// engines on one encrypted volume issue overlapping AES-GCM nonces, so the
// second claimant losing is the whole point of the lease.
func TestVolumeLease_SecondNodeIsRefused(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	first := newTestLeases(t, natsURL, "node-a")
	second := newTestLeases(t, natsURL, "node-b")

	const volumeName = "vol-leaseexclusion1"
	held, err := first.acquire(t.Context(), volumeName)
	require.NoError(t, err)
	require.NotNil(t, held)

	_, err = second.acquire(t.Context(), volumeName)
	require.ErrorIs(t, err, errVolumeLeaseHeld, "a second node must not get an engine on a volume this one holds")
	assert.Contains(t, err.Error(), "node-a", "the loser needs to know who to chase")
}

// TestVolumeLease_ReleasedVolumeIsClaimableAgain pins that exclusion is not a
// one-way door: a volume detached from one node has to be attachable on the
// next, which is the ordinary EBS lifecycle.
func TestVolumeLease_ReleasedVolumeIsClaimableAgain(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	first := newTestLeases(t, natsURL, "node-a")
	second := newTestLeases(t, natsURL, "node-b")

	const volumeName = "vol-leasehandover1"
	held, err := first.acquire(t.Context(), volumeName)
	require.NoError(t, err)
	first.release(t.Context(), held)

	taken, err := second.acquire(t.Context(), volumeName)
	require.NoError(t, err, "a released volume must be claimable by another node")
	require.Greater(t, taken.generation, held.generation, "generations must advance, or a stale writer is indistinguishable from a live one")
}

// TestVolumeLease_RepeatAcquireOnOneNodeShares covers the unmount seal, which
// opens a detached engine while the mount that is being torn down still holds
// the lease. Refusing itself there would fail every seal.
func TestVolumeLease_RepeatAcquireOnOneNodeShares(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	leases := newTestLeases(t, natsURL, "node-a")
	other := newTestLeases(t, natsURL, "node-b")

	const volumeName = "vol-leaserefcount1"
	mount, err := leases.acquire(t.Context(), volumeName)
	require.NoError(t, err)
	seal, err := leases.acquire(t.Context(), volumeName)
	require.NoError(t, err, "the same node must be able to open a second handle on a volume it already holds")
	require.Same(t, mount, seal, "a repeat acquisition must share the lease, not allocate a second one")

	// The seal finishing must not surrender the mount's claim.
	leases.release(t.Context(), seal)
	_, err = other.acquire(t.Context(), volumeName)
	require.ErrorIs(t, err, errVolumeLeaseHeld, "releasing one handle must not hand the volume to another node while the mount holds it")

	leases.release(t.Context(), mount)
	_, err = other.acquire(t.Context(), volumeName)
	require.NoError(t, err, "the last release must actually give the volume up")
}

// TestVolumeLease_NodeReclaimsItsOwnStaleEntry covers a daemon restart. The
// nbdkit exports outlive the daemon, so recovery has to re-adopt them; waiting
// out the TTL would leave live exports untracked.
func TestVolumeLease_NodeReclaimsItsOwnStaleEntry(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-leaserestart01"
	before := newTestLeases(t, natsURL, "node-a")
	stale, err := before.acquire(t.Context(), volumeName)
	require.NoError(t, err)

	// A restart, not a release: the entry stays behind with nothing renewing it.
	stale.stop()
	<-stale.done

	after := newTestLeases(t, natsURL, "node-a")
	reclaimed, err := after.acquire(t.Context(), volumeName)
	require.NoError(t, err, "a restarted daemon must reclaim the leases of the exports that outlived it")
	require.Greater(t, reclaimed.generation, stale.generation)
}

// TestVolumeLease_OpenRefusesWithoutALeaseStore pins the fail-closed default.
// A daemon that cannot reach JetStream cannot establish that it is the only
// opener, and opening blind is what the lease exists to prevent.
func TestVolumeLease_OpenRefusesWithoutALeaseStore(t *testing.T) {
	cfg := &Config{NodeName: "test-node"}

	lease, err := cfg.acquireVolumeLease(t.Context(), "vol-nostore00000001")
	require.Error(t, err, "no lease store must refuse the open, not wave it through")
	assert.Nil(t, lease)
}

// conflictKV fails every Update with a wrong-last-sequence response under the
// given code, which is how a renewal presents once another node has taken the
// lease over. Single-replica streams report 10071, replicated ones 10164.
type conflictKV struct {
	jetstream.KeyValue

	code jetstream.ErrorCode
}

func (k *conflictKV) Update(context.Context, string, []byte, uint64) (uint64, error) {
	apiErr := &jetstream.APIError{
		ErrorCode:   k.code,
		Code:        400,
		Description: "wrong last sequence: 3",
	}
	return 0, fmt.Errorf("%w: %w", apiErr, jetstream.ErrKeyRevisionMismatch)
}

// TestVolumeLease_RenewalConflictLosesTheLease is the multi-node regression: a
// renewal refused on a replicated bucket must mark the lease lost, not shrug it
// off as transient and keep renewing over whoever now holds the volume.
func TestVolumeLease_RenewalConflictLosesTheLease(t *testing.T) {
	for name, code := range map[string]jetstream.ErrorCode{
		"single replica": jetstream.JSErrCodeStreamWrongLastSequence,
		"replicated":     jetstream.JSErrCodeStreamWrongLastSequenceConstant,
	} {
		t.Run(name, func(t *testing.T) {
			_, natsURL := setupEmbeddedNATS(t)
			leases := newTestLeases(t, natsURL, "node-a")

			lease, err := leases.acquire(t.Context(), "vol-renewconflict1")
			require.NoError(t, err)
			lease.stop()
			<-lease.done

			leases.kv = &conflictKV{KeyValue: leases.kv, code: code}
			assert.False(t, lease.renew(t.Context()), "a refused renewal must not report the lease as still held")

			lease.mu.Lock()
			defer lease.mu.Unlock()
			assert.True(t, lease.lost, "a refused renewal must mark the lease lost so the renew loop stops")
		})
	}
}

// TestVolumeLease_RejectsUnsafeKeys pins that a volume name off the wire
// cannot address another volume's entry: "." and ">" are JetStream subject
// tokens, and a name carrying them would claim or read the wrong key.
func TestVolumeLease_RejectsUnsafeKeys(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	leases := newTestLeases(t, natsURL, "node-a")

	for _, name := range []string{"vol.other", "vol>", "", "vol/../other", "vol *"} {
		_, err := leases.acquire(t.Context(), name)
		require.Errorf(t, err, "volume name %q must not become a lease key", name)
	}
}
