package viperblockd

import (
	"context"
	"encoding/json"
	"errors"
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

	leases, err := newVolumeLeases(t.Context(), nc, owner, 1)
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
//
// Get is delegated unless getErr or getValue is set, so a test can say what the
// re-read finds: that is what decides whether a refused renewal was a takeover
// or this node's own write coming back.
type conflictKV struct {
	jetstream.KeyValue

	code     jetstream.ErrorCode
	getErr   error
	getValue []byte
}

func (k *conflictKV) Update(context.Context, string, []byte, uint64) (uint64, error) {
	apiErr := &jetstream.APIError{
		ErrorCode:   k.code,
		Code:        400,
		Description: "wrong last sequence: 3",
	}
	return 0, fmt.Errorf("%w: %w", apiErr, jetstream.ErrKeyRevisionMismatch)
}

func (k *conflictKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	switch {
	case k.getErr != nil:
		return nil, k.getErr
	case k.getValue != nil:
		return stubEntry{value: k.getValue, revision: 99}, nil
	}
	return k.KeyValue.Get(ctx, key)
}

// stubEntry is the two fields readopt looks at. The rest of the interface is
// never called, so it panics rather than returning a plausible zero value.
type stubEntry struct {
	jetstream.KeyValueEntry

	value    []byte
	revision uint64
}

func (e stubEntry) Value() []byte    { return e.value }
func (e stubEntry) Revision() uint64 { return e.revision }

// leaseEntry is what a holder's entry looks like on the wire.
func leaseEntry(t *testing.T, owner string, generation uint64) []byte {
	t.Helper()
	raw, err := json.Marshal(volumeLeaseRecord{Owner: owner, Generation: generation, AcquiredAt: time.Now().UTC()})
	require.NoError(t, err)
	return raw
}

// TestVolumeLease_RenewalConflictLosesTheLeaseToARealPeer is the multi-node
// regression: a renewal refused on a replicated bucket, where the entry really
// has been taken, must mark the lease lost rather than shrug it off as
// transient and keep renewing over whoever now holds the volume.
func TestVolumeLease_RenewalConflictLosesTheLeaseToARealPeer(t *testing.T) {
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

			leases.kv = &conflictKV{
				KeyValue: leases.kv,
				code:     code,
				getValue: leaseEntry(t, "node-b", lease.generation+7),
			}
			assert.False(t, lease.renew(t.Context()), "a refused renewal must not report the lease as still held")

			lease.mu.Lock()
			defer lease.mu.Unlock()
			assert.True(t, lease.lost, "a refused renewal must mark the lease lost so the renew loop stops")
		})
	}
}

// TestVolumeLease_RenewalConflictOnThisNodesOwnWriteIsNotATakeover is the P0.
//
// A renewal that times out is treated as transient and keeps the lease, but the
// server may have applied it anyway — leaving it a revision ahead of what the
// holder recorded, so every later attempt is refused for that reason alone. A
// ten-second JetStream stall on prod destroyed four running guests this way,
// each fenced with winner=unknown against a peer that never existed.
func TestVolumeLease_RenewalConflictOnThisNodesOwnWriteIsNotATakeover(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	leases := newTestLeases(t, natsURL, "node-a")

	const volumeName = "vol-renewreadopt"
	fenced := make(chan leaseLossKind, 1)
	leases.onLost = func(_ context.Context, _ string, kind leaseLossKind) { fenced <- kind }

	lease, err := leases.acquire(t.Context(), volumeName)
	require.NoError(t, err)
	lease.stop()
	<-lease.done

	before := lease.lastConfirmed()

	// The entry still names this node and this lease's generation, which is what
	// the server holds after applying a write the client never saw acknowledged.
	leases.kv = &conflictKV{
		KeyValue: leases.kv,
		code:     jetstream.JSErrCodeStreamWrongLastSequenceConstant,
		getValue: leaseEntry(t, "node-a", lease.generation),
	}

	assert.True(t, lease.renew(t.Context()),
		"a renewal refused against this node's own entry is not a takeover, so the lease is kept")

	lease.mu.Lock()
	defer lease.mu.Unlock()
	assert.False(t, lease.lost, "nothing took the volume, so nothing may be fenced")
	assert.Equal(t, uint64(99), lease.revision,
		"the revision the server actually holds has to be adopted or every later renewal is refused too")
	assert.Equal(t, before, lease.confirmed,
		"a re-read is not an acknowledged write, so it must not extend how long this holder may keep writing")

	select {
	case kind := <-fenced:
		t.Fatalf("the export was fenced (%v) over a volume nobody took", kind)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestVolumeLease_RenewalConflictOnAnOlderGenerationIsATakeover covers the same
// node holding the entry under a later lease — a daemon that restarted and
// reclaimed the volume through takeOver. The owner matches and the lease is
// still superseded, so the generation is what decides it.
func TestVolumeLease_RenewalConflictOnAnOlderGenerationIsATakeover(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	leases := newTestLeases(t, natsURL, "node-a")

	lease, err := leases.acquire(t.Context(), "vol-renewgeneration")
	require.NoError(t, err)
	lease.stop()
	<-lease.done

	leases.kv = &conflictKV{
		KeyValue: leases.kv,
		code:     jetstream.JSErrCodeStreamWrongLastSequenceConstant,
		getValue: leaseEntry(t, "node-a", lease.generation+1),
	}

	assert.False(t, lease.renew(t.Context()), "a later generation on this node supersedes this lease")

	lease.mu.Lock()
	defer lease.mu.Unlock()
	assert.True(t, lease.lost, "an entry this lease did not write is not this lease's to keep")
}

// TestVolumeLease_RenewalConflictWithAnUnreadableEntryKeepsTheLease is the
// other half of the rule. A re-read that cannot be answered is evidence of
// nothing, so it must not fence — and it must not silently extend the lease
// either. Local validity is the bound that depends on reading nothing.
func TestVolumeLease_RenewalConflictWithAnUnreadableEntryKeepsTheLease(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	leases := newTestLeases(t, natsURL, "node-a")

	lease, err := leases.acquire(t.Context(), "vol-renewunreadable")
	require.NoError(t, err)
	lease.stop()
	<-lease.done

	before := lease.lastConfirmed()
	leases.kv = &conflictKV{
		KeyValue: leases.kv,
		code:     jetstream.JSErrCodeStreamWrongLastSequenceConstant,
		getErr:   nats.ErrTimeout,
	}

	assert.True(t, lease.renew(t.Context()), "an unanswerable re-read is not evidence that the volume moved")

	lease.mu.Lock()
	defer lease.mu.Unlock()
	assert.False(t, lease.lost, "a fence needs evidence, and a timeout is not evidence")
	assert.Equal(t, before, lease.confirmed,
		"keeping the lease must not reset the clock that bounds how long it may be kept")
}

// TestVolumeLease_RenewalAgainstAMissingEntryIsATakeover covers the entry being
// gone rather than changed. There is nothing to re-read and nothing to adopt,
// and the server may already have granted it to somebody else.
func TestVolumeLease_RenewalAgainstAMissingEntryIsATakeover(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	leases := newTestLeases(t, natsURL, "node-a")

	lease, err := leases.acquire(t.Context(), "vol-renewmissing")
	require.NoError(t, err)
	lease.stop()
	<-lease.done

	leases.kv = &conflictKV{
		KeyValue: leases.kv,
		code:     jetstream.JSErrCodeStreamWrongLastSequenceConstant,
		getErr:   jetstream.ErrKeyNotFound,
	}

	assert.False(t, lease.renew(t.Context()), "an entry that is gone cannot show this node is the only writer")

	lease.mu.Lock()
	defer lease.mu.Unlock()
	assert.True(t, lease.lost, "a missing entry is a lost lease")
}

// TestVolumeLease_FencingRule states the rule itself, in one place, because the
// two candidate rules are incompatible and this is what has to outlive the
// argument between them.
//
// Fencing kills a running guest's disk. So it needs evidence, and a rejected
// write is not evidence — only a re-read showing the entry is no longer this
// lease's, or this holder's own clock saying it may no longer write. The weaker
// rule, that an unnameable winner never fences, would remove the second and
// leave a partitioned node writing for as long as it stayed partitioned.
func TestVolumeLease_FencingRule(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	leases := newTestLeases(t, natsURL, "node-a")

	lease, err := leases.acquire(t.Context(), "vol-fencingrule")
	require.NoError(t, err)
	lease.stop()
	<-lease.done

	// A revision mismatch may not fence before a re-read has shown the entry is
	// no longer this lease's.
	leases.kv = &conflictKV{
		KeyValue: leases.kv,
		code:     jetstream.JSErrCodeStreamWrongLastSequenceConstant,
		getValue: leaseEntry(t, "node-a", lease.generation),
	}
	require.True(t, lease.renew(t.Context()),
		"a revision mismatch may not fence before a re-read says the entry is no longer ours")

	// Lapsed local validity always fences, whether or not a winner can be named.
	// This is the only bound that holds when nothing can be read at all.
	require.False(t, lease.expiredLocally(), "a lease confirmed just now is still valid")
	lease.mu.Lock()
	lease.confirmed = time.Now().Add(-volumeLeaseValidity - time.Second)
	lease.mu.Unlock()
	require.True(t, lease.expiredLocally(),
		"a holder that cannot confirm its lease must stop writing on its own clock, named winner or not")
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

// TestLeaseStoreUnavailable separates "the store could not answer" from "the
// store answered no". Only the first is worth retrying, and getting it wrong in
// either direction is expensive: treating a held lease as transient would retry
// into a second engine on one encrypted volume, and treating a timeout as
// permanent strands a guest across a cluster restart, which is what happened.
func TestLeaseStoreUnavailable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"context deadline on a starting JetStream", context.DeadlineExceeded, true},
		{"wrapped deadline", fmt.Errorf("kv update: %w", context.DeadlineExceeded), true},
		{"no responders", nats.ErrNoResponders, true},
		{"request timeout", nats.ErrTimeout, true},
		{"connection closed", nats.ErrConnectionClosed, true},
		{"connection draining", nats.ErrConnectionDraining, true},
		{"no servers", nats.ErrNoServers, true},
		{"bucket not yet created", jetstream.ErrBucketNotFound, true},

		// The store was reachable and said no. Retrying cannot change it.
		{"key already exists", jetstream.ErrKeyExists, false},
		{"lease held by another owner", errVolumeLeaseHeld, false},
		{"unrelated failure", errors.New("marshal lease record"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, leaseStoreUnavailable(tt.err))
		})
	}
}

// TestVolumeLease_UnreachableStoreIsRetryable is the end of the chain that
// matters: an acquire against a store that cannot answer must surface as
// ErrLeaseStoreUnavailable, because that is the sentinel mountErrRetryable
// looks for and the only reason the relaunch backoff engages.
func TestVolumeLease_UnreachableStoreIsRetryable(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	leases, err := newVolumeLeases(t.Context(), nc, "node-a", 1)
	require.NoError(t, err)

	// Close the connection under the bound store, which is the shape of a
	// daemon that came back before NATS finished starting.
	nc.Close()

	_, err = leases.acquire(t.Context(), "vol-storedown1")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrLeaseStoreUnavailable,
		"a store that cannot answer must be reported as retryable, not as a refusal")
	assert.NotErrorIs(t, err, errVolumeLeaseHeld)
}
