package vpcpeering

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/migrate"
	"github.com/nats-io/nats.go/jetstream"
)

// errConflict reports a revision-checked write that lost its race; callers
// re-read and retry.
var errConflict = errors.New("vpcpeering: revision conflict")

// errLinkSpaceExhausted reports that every link slot is held.
var errLinkSpaceExhausted = errors.New("vpcpeering: no free link address")

// moveAttempts bounds the re-read loops of Accept, Reject, Delete and the moves.
const moveAttempts = 5

// Store holds the peering buckets and the VPC bucket the validity rules read.
// The service, the reconciler callbacks and the sweep all go through it.
type Store struct {
	live  jetstream.KeyValue
	term  jetstream.KeyValue
	vpcKV jetstream.KeyValue
	now   func() time.Time

	centralTags CentralTagStore
}

// NewStore wraps already-open buckets.
func NewStore(live, term, vpcKV jetstream.KeyValue) *Store {
	return &Store{live: live, term: term, vpcKV: vpcKV, now: time.Now}
}

// OpenStore opens or creates the peering buckets and opens the VPC bucket.
func OpenStore(ctx context.Context, js jetstream.JetStream) (*Store, error) {
	live, err := kvstore.GetOrCreateBucket(ctx, js, KVBucketPeeringConnections, 5)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", KVBucketPeeringConnections, err)
	}
	if err := migrate.DefaultRegistry.RunKV(ctx, KVBucketPeeringConnections, live, KVBucketPeeringConnectionsVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketPeeringConnections, err)
	}
	term, err := kvstore.GetOrCreateBucketWithOptions(ctx, js, kvstore.BucketOptions{
		Name:        KVBucketPeeringConnectionsTerminal,
		Description: "Terminal VPC peering connections, kept for AWS's visibility windows",
		History:     1,
		TTL:         terminalBucketTTL,
	})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", KVBucketPeeringConnectionsTerminal, err)
	}
	if err := migrate.DefaultRegistry.RunKV(ctx, KVBucketPeeringConnectionsTerminal, term, KVBucketPeeringConnectionsTerminalVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketPeeringConnectionsTerminal, err)
	}
	vpcKV, err := kvstore.GetOrCreateBucket(ctx, js, ec2vpc.KVBucketVPCs, 10)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", ec2vpc.KVBucketVPCs, err)
	}
	return NewStore(live, term, vpcKV), nil
}

// view is a connection as a reader sees it: the stored record plus the state
// the validity, expiry and VPC-absence rules derive from it at read time.
type view struct {
	rec          PeeringRecord
	rev          uint64 // live revision; 0 when read from the terminal bucket
	status       string
	message      string
	terminalAt   time.Time
	terminatedBy string
}

// isTerminal reports an ended connection. A deleted one is ended to its
// readers while its cleanup is still pending.
func (v *view) isTerminal() bool {
	switch v.status {
	case StateFailed, StateRejected, StateExpired, StateDeleted:
		return true
	}
	return false
}

// visibleTo applies AWS's per-viewer windows to a terminal connection: two
// hours to the account that ended it (and for failed, to the requester), two
// days to the other owner, and two days to both for an expired request.
func (v *view) visibleTo(viewer string, now time.Time) bool {
	if !v.rec.involves(viewer) {
		return false
	}
	if !v.isTerminal() {
		return true
	}
	window := terminalLongWindow
	switch v.status {
	case StateFailed:
		if viewer != v.rec.RequesterAccountID {
			return false
		}
		window = terminalShortWindow
	case StateRejected, StateDeleted:
		if viewer == v.terminatedBy {
			window = terminalShortWindow
		}
	}
	return now.Sub(v.terminalAt) < window
}

func getJSON[T any](ctx context.Context, kv jetstream.KeyValue, key string) (*T, uint64, error) {
	entry, err := kv.Get(ctx, key)
	if err != nil {
		// A key KV cannot hold names nothing: a malformed caller-supplied ID
		// is absent, as an unknown one is.
		if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrInvalidKey) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("read %s: %w", key, err)
	}
	var v T
	if err := json.Unmarshal(entry.Value(), &v); err != nil {
		return nil, 0, fmt.Errorf("decode %s: %w", key, err)
	}
	return &v, entry.Revision(), nil
}

func putJSON(ctx context.Context, kv jetstream.KeyValue, key string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", key, err)
	}
	if _, err := kv.Put(ctx, key, data); err != nil {
		return fmt.Errorf("write %s: %w", key, err)
	}
	return nil
}

// writeJSON creates key when rev is 0, else updates it at rev; a lost race
// is errConflict.
func writeJSON(ctx context.Context, kv jetstream.KeyValue, key string, v any, rev uint64) (uint64, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return 0, fmt.Errorf("encode %s: %w", key, err)
	}
	if rev == 0 {
		rev, err = kv.Create(ctx, key, data)
	} else {
		rev, err = kv.Update(ctx, key, data, rev)
	}
	if kvstore.IsRevisionConflict(err) {
		return 0, errConflict
	}
	if err != nil {
		return 0, fmt.Errorf("write %s: %w", key, err)
	}
	return rev, nil
}

// deleteAt removes key at rev; an absent key is already deleted.
func deleteAt(ctx context.Context, kv jetstream.KeyValue, key string, rev uint64) error {
	err := kv.Delete(ctx, key, jetstream.LastRevision(rev))
	switch {
	case err == nil, errors.Is(err, jetstream.ErrKeyNotFound):
		return nil
	case kvstore.IsRevisionConflict(err):
		return errConflict
	}
	return fmt.Errorf("delete %s: %w", key, err)
}

func deleteKey(ctx context.Context, kv jetstream.KeyValue, key string) error {
	if err := kv.Delete(ctx, key); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("delete %s: %w", key, err)
	}
	return nil
}

// vpcRecord reads accountID's VPC; nil when absent.
func (s *Store) vpcRecord(ctx context.Context, accountID, vpcID string) (*ec2vpc.VPCRecord, error) {
	rec, _, err := getJSON[ec2vpc.VPCRecord](ctx, s.vpcKV, kvstore.AccountKey(accountID, vpcID))
	return rec, err
}

// missingVpcOwner returns the owner of whichever of the connection's VPCs is
// absent, or "" when both exist.
func (s *Store) missingVpcOwner(ctx context.Context, rec *PeeringRecord) (string, error) {
	for _, side := range [][2]string{{rec.RequesterAccountID, rec.RequesterVpcID}, {rec.AccepterAccountID, rec.AccepterVpcID}} {
		v, err := s.vpcRecord(ctx, side[0], side[1])
		if err != nil {
			return "", err
		}
		if v == nil {
			return side[0], nil
		}
	}
	return "", nil
}

// reqClaimNames reports whether the directed request claim names id.
func (s *Store) reqClaimNames(ctx context.Context, rec *PeeringRecord) (bool, error) {
	c, _, err := getJSON[claim](ctx, s.live, reqKey(rec.RequesterVpcID, rec.AccepterVpcID))
	if err != nil {
		return false, err
	}
	return c != nil && c.PcxID == rec.ID, nil
}

// load returns the connection as readers see it, or nil when it is absent or
// is a pending record no request claim names.
func (s *Store) load(ctx context.Context, id string) (*view, error) {
	rec, rev, err := getJSON[PeeringRecord](ctx, s.live, recordKey(id))
	if err != nil {
		return nil, err
	}
	if rec != nil {
		return s.liveView(ctx, rec, rev)
	}
	rec, _, err = getJSON[PeeringRecord](ctx, s.term, recordKey(id))
	if err != nil || rec == nil {
		return nil, err
	}
	return &view{rec: *rec, status: rec.Status, message: rec.StatusMessage, terminalAt: rec.TerminalAt, terminatedBy: rec.TerminatedBy}, nil
}

// liveView derives a live record's read-time state.
func (s *Store) liveView(ctx context.Context, rec *PeeringRecord, rev uint64) (*view, error) {
	v := &view{rec: *rec, rev: rev, status: rec.Status, message: rec.StatusMessage, terminalAt: rec.TerminalAt, terminatedBy: rec.TerminatedBy}
	if rec.Status != StatePendingAcceptance {
		return v, nil
	}
	valid, err := s.reqClaimNames(ctx, rec)
	if err != nil {
		return nil, err
	}
	if !valid {
		return nil, nil
	}
	now := s.now()
	if !rec.ExpirationTime.IsZero() && !now.Before(rec.ExpirationTime) {
		v.status, v.message = StateExpired, expiredMessage
		v.terminalAt, v.terminatedBy = rec.ExpirationTime, ""
		return v, nil
	}
	owner, err := s.missingVpcOwner(ctx, rec)
	if err != nil {
		return nil, err
	}
	switch {
	case owner == "":
	case owner == rec.RequesterAccountID && rec.AccepterAccountID != owner:
		// AWS deletes a request to another account whose requester VPC is deleted.
		v.status, v.message = StateDeleted, deletedMessage(owner)
		v.terminalAt, v.terminatedBy = now, owner
	default:
		v.status, v.message = StateRejected, rejectedMessage(owner)
		v.terminalAt, v.terminatedBy = now, owner
	}
	return v, nil
}

// ended is the record a view's derived terminal state is stored as.
func (v *view) ended() PeeringRecord {
	rec := v.rec
	rec.Status, rec.StatusMessage = v.status, v.message
	rec.TerminalAt, rec.TerminatedBy = v.terminalAt, v.terminatedBy
	rec.Generation++
	return rec
}

// confirmCreated fences the sweep once rec's claim is won: a sweep that read
// the record or an index unclaimed deletes at that revision, so rewriting them
// makes it lose. It reports false when the sweep already removed the record.
func (s *Store) confirmCreated(ctx context.Context, rec *PeeringRecord, rev uint64) (bool, error) {
	if _, err := writeJSON(ctx, s.live, recordKey(rec.ID), rec, rev); errors.Is(err, errConflict) {
		v, err := s.load(ctx, rec.ID)
		if err != nil || v == nil || v.rev == 0 {
			return v != nil, err
		}
	} else if err != nil {
		return false, err
	}
	return true, s.writeIndexes(ctx, rec)
}

// isLinkedAlive reports a view that holds a link: provisioning or active.
func (v *view) isLinkedAlive() bool {
	return v.status == StateProvisioning || v.status == StateActive
}

// isOpen reports a view a repeated Create returns and a route may target.
func (v *view) isOpen() bool {
	return v.status == StatePendingAcceptance || v.isLinkedAlive()
}

// claimJudge decides a claim another connection holds: keep leaves it to its
// holder, retry re-reads it after the judge cleared the way, and neither takes
// it over at the revision read.
type claimJudge func(ctx context.Context, held claim) (keep, retry bool, err error)

// takeClaim takes key for id, refreshing its own claim or taking over one the
// judge releases. It returns the ID holding the claim afterwards.
func (s *Store) takeClaim(ctx context.Context, key, id string, judge claimJudge) (string, error) {
	mine := claim{PcxID: id, ClaimedAt: s.now()}
	for range moveAttempts {
		if _, err := writeJSON(ctx, s.live, key, mine, 0); !errors.Is(err, errConflict) {
			return id, err
		}
		held, rev, err := getJSON[claim](ctx, s.live, key)
		if err != nil {
			return "", err
		}
		if held == nil {
			continue
		}
		if held.PcxID != id {
			keep, retry, err := judge(ctx, *held)
			if err != nil || keep {
				return held.PcxID, err
			}
			if retry {
				continue
			}
		}
		if _, err := writeJSON(ctx, s.live, key, mine, rev); !errors.Is(err, errConflict) {
			return id, err
		}
	}
	return "", fmt.Errorf("take %s: %w", key, errConflict)
}

// takeReqClaim takes the directed request claim for rec. It returns the ID
// that holds it afterwards: rec.ID, or the valid connection that already did.
func (s *Store) takeReqClaim(ctx context.Context, rec *PeeringRecord) (string, error) {
	return s.takeClaim(ctx, reqKey(rec.RequesterVpcID, rec.AccepterVpcID), rec.ID, func(ctx context.Context, held claim) (bool, bool, error) {
		owner, err := s.load(ctx, held.PcxID)
		if err != nil || owner == nil || owner.isOpen() {
			return owner != nil, false, err
		}
		// An expired request, or one whose VPC is gone, keeps its visibility
		// window: move it to the terminal bucket before its claim is taken.
		if owner.rec.Status != StatePendingAcceptance {
			return false, false, nil
		}
		ended := owner.ended()
		if err := s.moveTerminal(ctx, &ended, owner.rev); err != nil && !errors.Is(err, errConflict) {
			return false, false, err
		}
		return false, true, nil
	})
}

// errAbsent marks a record read-modify-write whose record is gone.
var errAbsent = errors.New("vpcpeering: record absent")

// errClaimHeld reports a pair claim held by another connection that may keep it.
var errClaimHeld = errors.New("vpcpeering: claim held")

// takeOwnedClaim takes key for id or takes over a claim stale judges by the
// holder's live record (nil when absent) and claim age. A pending holder is
// fenced at the revision read before the takeover.
func (s *Store) takeOwnedClaim(ctx context.Context, key, id string, stale func(holder *PeeringRecord, age time.Duration) bool) error {
	holderID, err := s.takeClaim(ctx, key, id, func(ctx context.Context, held claim) (bool, bool, error) {
		holder, rev, err := getJSON[PeeringRecord](ctx, s.live, recordKey(held.PcxID))
		if err != nil || !stale(holder, s.now().Sub(held.ClaimedAt)) {
			return err == nil, false, err
		}
		if holder == nil || holder.Status != StatePendingAcceptance {
			return false, false, nil
		}
		holder.Generation++
		_, err = writeJSON(ctx, s.live, recordKey(holder.ID), holder, rev)
		if errors.Is(err, errConflict) {
			return false, true, nil
		}
		return false, false, err
	})
	if err == nil && holderID != id {
		return errClaimHeld
	}
	return err
}

// pairClaimStale: the holder is gone, terminal or deleted, or is a pending
// record whose claim is older than the grace, which is an Accept that crashed.
func pairClaimStale(holder *PeeringRecord, age time.Duration) bool {
	if holder == nil || holder.Status == StateDeleted || holder.isTerminalStatus() {
		return true
	}
	return holder.Status == StatePendingAcceptance && age >= claimGrace
}

// linkClaimStale: past the grace, the holder is gone, terminal, or does not
// carry this slot's prefix.
func linkClaimStale(slot string) func(*PeeringRecord, time.Duration) bool {
	return func(holder *PeeringRecord, age time.Duration) bool {
		if age < claimGrace {
			return false
		}
		return holder == nil || holder.isTerminalStatus() || holder.LinkPrefix != slot
	}
}

// takeLinkSlot claims a /30 for id, probing from a slot derived from the ID so
// a retried Accept finds its own claim first.
func (s *Store) takeLinkSlot(ctx context.Context, id string) (string, error) {
	start := linkStart(id)
	for i := range linkSlots {
		n := (start + i) % linkSlots
		prefix := linkPrefix(n).String()
		err := s.takeOwnedClaim(ctx, linkKey(n), id, linkClaimStale(prefix))
		if err == nil {
			return prefix, nil
		}
		if !errors.Is(err, errClaimHeld) {
			return "", err
		}
	}
	return "", errLinkSpaceExhausted
}

// linkStart is the slot id's link search starts from.
func linkStart(id string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return int(h.Sum32() % linkSlots)
}

// releaseClaim deletes key at its revision while it still names id.
func (s *Store) releaseClaim(ctx context.Context, key, id string) error {
	held, rev, err := getJSON[claim](ctx, s.live, key)
	if err != nil || held == nil || held.PcxID != id {
		return err
	}
	if err := deleteAt(ctx, s.live, key, rev); err != nil && !errors.Is(err, errConflict) {
		return err
	}
	return nil
}

// releaseLinkClaim releases the link slot a record carries.
func (s *Store) releaseLinkClaim(ctx context.Context, rec *PeeringRecord) error {
	if rec.LinkPrefix == "" {
		return nil
	}
	for n := range linkSlots {
		if linkPrefix(n).String() == rec.LinkPrefix {
			return s.releaseClaim(ctx, linkKey(n), rec.ID)
		}
	}
	return nil
}

// writeIndexes writes the account and VPC index keys of a new connection.
func (s *Store) writeIndexes(ctx context.Context, rec *PeeringRecord) error {
	entry := indexEntry{CreatedAt: rec.CreatedAt}
	for _, acct := range rec.accounts() {
		if err := putJSON(ctx, s.live, acctKey(acct, rec.ID), entry); err != nil {
			return err
		}
	}
	for _, vpcID := range []string{rec.RequesterVpcID, rec.AccepterVpcID} {
		if err := putJSON(ctx, s.live, vpcKey(vpcID, rec.ID), entry); err != nil {
			return err
		}
	}
	return nil
}

// deleteIndexes removes the live index keys of a connection.
func (s *Store) deleteIndexes(ctx context.Context, rec *PeeringRecord) error {
	keys := []string{vpcKey(rec.RequesterVpcID, rec.ID), vpcKey(rec.AccepterVpcID, rec.ID)}
	for _, acct := range rec.accounts() {
		keys = append(keys, acctKey(acct, rec.ID))
	}
	for _, k := range keys {
		if err := deleteKey(ctx, s.live, k); err != nil {
			return err
		}
	}
	return nil
}

// writeTerminal stores rec and its account keys in the terminal bucket.
func (s *Store) writeTerminal(ctx context.Context, rec *PeeringRecord) error {
	if err := putJSON(ctx, s.term, recordKey(rec.ID), rec); err != nil {
		return err
	}
	entry := indexEntry{CreatedAt: rec.CreatedAt}
	viewers := rec.accounts()
	// Only the requester sees a failed request, so no other account's reads
	// grow with failed requests made against it.
	if rec.Status == StateFailed {
		viewers = []string{rec.RequesterAccountID}
	}
	for _, acct := range viewers {
		if err := putJSON(ctx, s.term, acctKey(acct, rec.ID), entry); err != nil {
			return err
		}
	}
	return nil
}

// finishMove moves a live record already written in a terminal status to the
// terminal bucket and releases its claims and index keys. Every step is
// idempotent, so a crash part way is finished by the next caller or the sweep.
func (s *Store) finishMove(ctx context.Context, rec *PeeringRecord, rev uint64) error {
	if err := s.writeTerminal(ctx, rec); err != nil {
		return err
	}
	if err := deleteAt(ctx, s.live, recordKey(rec.ID), rev); err != nil {
		return err
	}
	return s.releaseAll(ctx, rec)
}

// releaseAll releases every claim naming rec and deletes its index keys.
func (s *Store) releaseAll(ctx context.Context, rec *PeeringRecord) error {
	if err := s.releaseClaim(ctx, reqKey(rec.RequesterVpcID, rec.AccepterVpcID), rec.ID); err != nil {
		return err
	}
	if err := s.releaseClaim(ctx, pairKey(rec.RequesterVpcID, rec.AccepterVpcID), rec.ID); err != nil {
		return err
	}
	if err := s.releaseLinkClaim(ctx, rec); err != nil {
		return err
	}
	return s.deleteIndexes(ctx, rec)
}

// moveTerminal writes the live record in its terminal status at rev, then
// finishes the move. The first write is the fence: a racing writer at the
// same revision loses.
func (s *Store) moveTerminal(ctx context.Context, rec *PeeringRecord, rev uint64) error {
	newRev, err := writeJSON(ctx, s.live, recordKey(rec.ID), rec, rev)
	if err != nil {
		return err
	}
	return s.finishMove(ctx, rec, newRev)
}

// listIndex returns the pcx IDs under prefix (a "acct.<a>." or "vpc.<v>."
// key prefix) in kv.
func listIndex(ctx context.Context, kv jetstream.KeyValue, prefix string) ([]string, error) {
	lister, err := kv.ListKeysFiltered(ctx, prefix+"*")
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("list %s: %w", prefix, err)
	}
	var ids []string
	for key := range lister.Keys() {
		if id := idFromIndexKey(key); id != "" {
			ids = append(ids, id)
		}
	}
	// Cancellation closes the lister early, and a short list must not read as complete.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}
