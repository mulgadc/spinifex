package snapshot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
	otelsetup "github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
)

// ClaimPrefix is the kvsync prefix of every snapshot's claim, ClaimPrefix+ID,
// naming the node that alone may call the provider for it or delete its
// document. An AMI's claim, keyed by image ID, shares the prefix.
const ClaimPrefix = "snapshot."

// claimReleaseTimeout bounds the unlock after the last write, which runs even
// when the request that held the claim has been cancelled.
const claimReleaseTimeout = 5 * time.Second

// NewClaimGroup returns node's group of snapshot claims over store. The daemon
// builds it once per process, as kvsync requires of every prefix.
func NewClaimGroup(store kvsync.KVStore, node string) *kvsync.ClaimGroup {
	return kvsync.NewClaimGroup(store, ClaimPrefix, node)
}

// NodeLiveness reports when a node last heartbeated. found is false only for a
// node with no heartbeat; a read that fails is an error, never a dead node.
type NodeLiveness interface {
	LastSeen(ctx context.Context, node string) (seen time.Time, found bool, err error)
}

// errClaimHeld is a snapshot claim another holder has: a request of this
// process, or a node whose heartbeat is still fresh.
var errClaimHeld = errors.New("snapshot claim is held")

// Claims decides who may act on a pending snapshot. A kvsync claim names a
// node, not a process, so the claims this process holds are also kept here:
// the reaper on the node of a running request must not adopt that request's.
type Claims struct {
	group    *kvsync.ClaimGroup
	node     string
	liveness NodeLiveness
	now      func() time.Time

	mu    sync.Mutex
	local map[string]struct{}
}

// NewClaims returns the claims of node over group. A nil liveness never judges
// a holder dead, so a claim another node holds is never taken over.
func NewClaims(group *kvsync.ClaimGroup, node string, liveness NodeLiveness) *Claims {
	return &Claims{group: group, node: node, liveness: liveness, now: time.Now, local: map[string]struct{}{}}
}

// localClaims excludes only within this process: a service built without the
// daemon's claim group has no other process to exclude.
func localClaims() *Claims {
	return NewClaims(nil, "", nil)
}

// heldClaim is one acquisition of a snapshot's claim.
type heldClaim struct {
	claims     *Claims
	snapshotID string
	claim      *kvsync.Claim
	token      kvsync.Token
}

// reserve marks snapshotID held by this process, and is false when it already is.
func (c *Claims) reserve(snapshotID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, held := c.local[snapshotID]; held {
		return false
	}
	c.local[snapshotID] = struct{}{}
	return true
}

func (c *Claims) unreserve(snapshotID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.local, snapshotID)
}

// lock takes snapshotID's claim for a creating request. It never takes over:
// the ID is new, so any holder is a fault worth refusing the request for.
func (c *Claims) lock(ctx context.Context, snapshotID string) (*heldClaim, error) {
	return c.acquire(ctx, snapshotID, false)
}

// take acquires snapshotID's claim for the reaper. A claim another node holds
// is taken over only once that node's heartbeat is LivenessWindow old or
// absent; while it is fresh, or cannot be read, the claim is refused.
func (c *Claims) take(ctx context.Context, snapshotID string) (*heldClaim, error) {
	return c.acquire(ctx, snapshotID, true)
}

// acquire reserves snapshotID in this process and locks its claim, taking it
// over from a dead holder only when takeOver allows.
func (c *Claims) acquire(ctx context.Context, snapshotID string, takeOver bool) (*heldClaim, error) {
	if !c.reserve(snapshotID) {
		return nil, fmt.Errorf("%w: %s by this process", errClaimHeld, snapshotID)
	}
	held, err := c.acquireReserved(ctx, snapshotID, takeOver)
	if err != nil {
		c.unreserve(snapshotID)
		return nil, err
	}
	return held, nil
}

func (c *Claims) acquireReserved(ctx context.Context, snapshotID string, takeOver bool) (*heldClaim, error) {
	held := &heldClaim{claims: c, snapshotID: snapshotID}
	if c.group == nil {
		return held, nil
	}
	held.claim = c.group.Get(snapshotID)
	tok, err := held.claim.TryLock(ctx)
	if err == nil {
		held.token = tok
		return held, nil
	}
	he, ok := errors.AsType[*kvsync.HeldError](err)
	if !takeOver || !ok || he.Current.Holder == "" {
		return nil, fmt.Errorf("take snapshot claim %s: %w", snapshotID, err)
	}
	holder := he.Current
	if c.liveness == nil {
		return nil, fmt.Errorf("%w: %s by %s, and no liveness is wired to judge it", errClaimHeld, snapshotID, holder.Holder)
	}
	seen, found, err := c.liveness.LastSeen(ctx, holder.Holder)
	if err != nil {
		return nil, fmt.Errorf("%w: %s by %s, whose heartbeat cannot be read: %w", errClaimHeld, snapshotID, holder.Holder, err)
	}
	now := c.now()
	if found && clusterv1.LiveAt(seen, now) {
		return nil, fmt.Errorf("%w: %s by %s, which is still heartbeating", errClaimHeld, snapshotID, holder.Holder)
	}
	tok, err = held.claim.TakeOver(ctx, holder)
	if err != nil {
		return nil, fmt.Errorf("take over snapshot claim %s from %s: %w", snapshotID, holder.Holder, err)
	}
	attrs := []any{"snapshotId", snapshotID, "holder", holder.Holder, "fromGeneration", holder.Generation, "generation", tok.Generation}
	if found {
		attrs = append(attrs, "heartbeat_age_ms", otelsetup.Millis(now.Sub(seen)))
	}
	slog.WarnContext(ctx, "Took over a pending snapshot's claim from a node whose heartbeat has stopped", attrs...)
	held.token = tok
	return held, nil
}

// release gives the claim up after the holder's last write. A failed unlock
// leaves the claim with this node until a restart or a takeover frees it, so
// it is logged and not returned.
func (h *heldClaim) release(ctx context.Context) {
	defer h.claims.unreserve(h.snapshotID)
	if h.claim == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), claimReleaseTimeout)
	defer cancel()
	if err := h.claim.Unlock(ctx, h.token); err != nil {
		slog.WarnContext(ctx, "Failed to release a snapshot claim",
			"snapshotId", h.snapshotID, "generation", h.token.Generation, "err", err)
	}
}

// releaseOwn unlocks snapshotID's claim when it names this node and no request
// of this process holds it: a claim left by an earlier process of this node,
// whose requests did not survive its restart.
func (c *Claims) releaseOwn(ctx context.Context, snapshotID string) (bool, error) {
	if c.group == nil || !c.reserve(snapshotID) {
		return false, nil
	}
	defer c.unreserve(snapshotID)
	claim := c.group.Get(snapshotID)
	tok, ok, err := claim.Holder(ctx)
	if err != nil {
		return false, fmt.Errorf("read snapshot claim %s: %w", snapshotID, err)
	}
	if !ok || tok.Holder != c.node {
		return false, nil
	}
	if err := claim.Unlock(ctx, tok); err != nil {
		if errors.Is(err, kvsync.ErrLost) {
			return false, nil
		}
		return false, fmt.Errorf("release snapshot claim %s: %w", snapshotID, err)
	}
	return true, nil
}
