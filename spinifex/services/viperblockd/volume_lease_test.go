package viperblockd

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
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

// TestVolumeLease_UnmountedSnapshotIsRefusedByTheHolder is the bead's case: a
// snapshot request that lands on a node without the volume mounted must not
// open a second engine over storage another node is writing.
func TestVolumeLease_UnmountedSnapshotIsRefusedByTheHolder(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-snapshotexcl01"
	holder := newTestLeases(t, natsURL, "node-a")
	_, err := holder.acquire(t.Context(), volumeName)
	require.NoError(t, err)

	// node-b has nothing mounted, so this is the unmounted branch.
	cfg := &Config{NodeName: "node-b", BaseDir: t.TempDir(), leases: newTestLeases(t, natsURL, "node-b")}
	snapshot, err := snapshotVolumeEngine(t.Context(), cfg, volumeName, "snap-excl0000000001")

	require.ErrorIs(t, err, errVolumeLeaseHeld, "a snapshot must not open an engine on a volume another node holds")
	assert.Nil(t, snapshot)
	assert.Equal(t, ebsprovider.ErrorCodeUnavailable, snapshotErrorCode(err),
		"the holder detaches eventually, so the caller has to be told this is worth retrying")
	assert.Empty(t, dirEntries(t, cfg.BaseDir), "the refusal must come before any engine touches the volume's directory")
}

// TestVolumeLease_UnmountedSnapshotRefusesWithoutALeaseStore pins the
// fail-closed default on the snapshot path specifically. A daemon that cannot
// reach JetStream cannot establish that it is the only opener.
func TestVolumeLease_UnmountedSnapshotRefusesWithoutALeaseStore(t *testing.T) {
	cfg := &Config{NodeName: "node-a", BaseDir: t.TempDir()}

	snapshot, err := snapshotVolumeEngine(t.Context(), cfg, "vol-snapshotnostor", "snap-nostore00000001")

	require.ErrorIs(t, err, errNoVolumeLeaseStore, "unprovable exclusion must refuse the snapshot, not wave it through")
	assert.Nil(t, snapshot)
	assert.Equal(t, ebsprovider.ErrorCodeUnavailable, snapshotErrorCode(err))
	assert.Empty(t, dirEntries(t, cfg.BaseDir))
}

// dirEntries lists dir, which a refused open must have left alone.
func dirEntries(t *testing.T, dir string) []os.DirEntry {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	return entries
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

// TestVolumeLease_ValidityIsBoundedBelowTheServerTTL pins the arithmetic that
// makes this a lease rather than a hint. The holder has to stop writing before
// the server may grant the entry to somebody else, and it has to get at least
// one check in between giving up on renewal and reaching that point.
func TestVolumeLease_ValidityIsBoundedBelowTheServerTTL(t *testing.T) {
	require.Less(t, volumeLeaseValidity, volumeLeaseTTL,
		"a holder that stops only when the server expires the entry has already overlapped its successor")
	require.Less(t, volumeLeaseRenewInterval, volumeLeaseValidity,
		"a renewal that comes due after validity lapses can never confirm one in time")
	require.Less(t, volumeLeaseCheckInterval, volumeLeaseValidity-volumeLeaseRenewInterval,
		"validity has to be tested at least once between a missed renewal and the deadline it enforces")
	require.Less(t, volumeLeaseRenewTimeout, volumeLeaseCheckInterval+volumeLeaseRenewInterval,
		"an unbounded renewal holds the goroutine past the point the entry may have been re-granted")
}

// TestVolumeLease_UnconfirmedRenewalSurrendersTheVolume is the partition case
// revision checking cannot see. A node that loses NATS while the winner keeps
// it never gets an update rejected, so nothing tells it the volume moved. It
// has to give the volume up on its own clock, and report the reason as a stall
// rather than as a peer taking it — nobody has necessarily taken it yet.
func TestVolumeLease_UnconfirmedRenewalSurrendersTheVolume(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	leases := newTestLeases(t, natsURL, "node-a")

	const volumeName = "vol-leasestalled"
	lost := make(chan leaseLossKind, 1)
	leases.onLost = func(_ context.Context, volume string, kind leaseLossKind) {
		assert.Equal(t, volumeName, volume)
		lost <- kind
	}

	lease, err := leases.acquire(t.Context(), volumeName)
	require.NoError(t, err)
	lease.stop()
	<-lease.done

	require.False(t, lease.expiredLocally(), "a lease confirmed just now is still valid")

	// Age the last confirmation past the validity window, which is what a node
	// that cannot reach JetStream looks like from inside.
	lease.mu.Lock()
	lease.confirmed = time.Now().Add(-volumeLeaseValidity - time.Second)
	lease.mu.Unlock()
	require.True(t, lease.expiredLocally(), "an unconfirmed holder must not still consider itself valid")

	lease.surrender(t.Context())

	select {
	case kind := <-lost:
		assert.Equal(t, leaseLostStalled, kind,
			"a stall is not a takeover: reporting it as one would name a winner that may not exist")
	case <-time.After(5 * time.Second):
		t.Fatal("a surrendered lease must fence its export, and nothing was called")
	}

	lease.mu.Lock()
	defer lease.mu.Unlock()
	assert.True(t, lease.lost, "a surrendered lease must be marked lost so release cannot delete the successor's entry")
}

// TestVolumeLease_SurrenderedLeaseIsNotDeletedOnRelease covers what happens
// after. The entry may already belong to another node, so deleting it on the
// way out would evict a live holder and hand the volume to a third opener.
func TestVolumeLease_SurrenderedLeaseIsNotDeletedOnRelease(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	leases := newTestLeases(t, natsURL, "node-a")

	const volumeName = "vol-leasestalledrelease"
	lease, err := leases.acquire(t.Context(), volumeName)
	require.NoError(t, err)

	lease.mu.Lock()
	lease.confirmed = time.Now().Add(-volumeLeaseValidity - time.Second)
	lease.mu.Unlock()
	lease.surrender(t.Context())

	leases.release(t.Context(), lease)

	_, err = leases.kv.Get(t.Context(), volumeName)
	require.NoError(t, err, "releasing a surrendered lease must leave the entry alone: it may be the successor's now")
}
