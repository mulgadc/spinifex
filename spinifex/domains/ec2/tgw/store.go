package tgw

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
var errConflict = errors.New("tgw: revision conflict")

// errClaimHeld reports a claim another live attachment keeps.
var errClaimHeld = errors.New("tgw: claim held")

// errLinkSpaceExhausted reports that every link slot is held.
var errLinkSpaceExhausted = errors.New("tgw: no free link address")

// Store holds the Transit Gateway buckets and the VPC capability admission
// fences on. The service, the reconciler callbacks and the sweep use it.
type Store struct {
	tgws    jetstream.KeyValue
	atts    jetstream.KeyValue
	term    jetstream.KeyValue
	parents *ec2vpc.Parents
	now     func() time.Time
	slots   int

	centralTags CentralTagStore
	accounts    AccountStatus
}

// NewStore wraps already-open buckets.
func NewStore(tgws, atts, term jetstream.KeyValue, parents *ec2vpc.Parents) *Store {
	return &Store{tgws: tgws, atts: atts, term: term, parents: parents, now: time.Now, slots: linkSlots}
}

// OpenStore opens or creates the Transit Gateway buckets and opens the VPC
// and subnet buckets.
func OpenStore(ctx context.Context, js jetstream.JetStream) (*Store, error) {
	tgws, err := openBucket(ctx, js, KVBucketTransitGateways, KVBucketTransitGatewaysVersion)
	if err != nil {
		return nil, err
	}
	atts, err := openBucket(ctx, js, KVBucketAttachments, KVBucketAttachmentsVersion)
	if err != nil {
		return nil, err
	}
	term, err := kvstore.GetOrCreateBucketWithOptions(ctx, js, kvstore.BucketOptions{
		Name:        KVBucketTerminal,
		Description: "Deleted and failed Transit Gateways and attachments, kept for AWS's visibility windows",
		History:     1,
		TTL:         terminalBucketTTL,
	})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", KVBucketTerminal, err)
	}
	if err := migrate.DefaultRegistry.RunKV(ctx, KVBucketTerminal, term, KVBucketTerminalVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketTerminal, err)
	}
	parents, err := ec2vpc.OpenParents(ctx, js)
	if err != nil {
		return nil, err
	}
	return NewStore(tgws, atts, term, parents), nil
}

func openBucket(ctx context.Context, js jetstream.JetStream, name string, version int) (jetstream.KeyValue, error) {
	kv, err := kvstore.GetOrCreateBucket(ctx, js, name, 5)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	if err := migrate.DefaultRegistry.RunKV(ctx, name, kv, version); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", name, err)
	}
	return kv, nil
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

// putJSON writes key and returns the revision it wrote.
func putJSON(ctx context.Context, kv jetstream.KeyValue, key string, v any) (uint64, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return 0, fmt.Errorf("encode %s: %w", key, err)
	}
	rev, err := kv.Put(ctx, key, data)
	if err != nil {
		return 0, fmt.Errorf("write %s: %w", key, err)
	}
	return rev, nil
}

// touchIndexes rewrites each index key at the revision its owner wrote, after
// the owner's record write. A sweep that judged the key ownerless meanwhile
// either removed it, and the rewrite fails, or loses its delete at that revision.
func touchIndexes(ctx context.Context, kv jetstream.KeyValue, revs map[string]uint64) error {
	for key, rev := range revs {
		if err := touchAt(ctx, kv, key, rev); err != nil {
			return fmt.Errorf("index %s: %w", key, err)
		}
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

// allKeys lists kv's keys; an empty bucket has none.
func allKeys(ctx context.Context, kv jetstream.KeyValue) ([]string, error) {
	keys, err := kvstore.Keys(ctx, kv)
	if errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, nil
	}
	return keys, err
}

// listIndex returns the IDs under prefix in kv.
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

// transitGateway reads a live Transit Gateway; nil when absent.
func (s *Store) transitGateway(ctx context.Context, accountID, id string) (*TransitGatewayRecord, uint64, error) {
	return getJSON[TransitGatewayRecord](ctx, s.tgws, recordKey(accountID, id))
}

// attachment reads a live attachment; nil when absent.
func (s *Store) attachment(ctx context.Context, accountID, id string) (*AttachmentRecord, uint64, error) {
	return getJSON[AttachmentRecord](ctx, s.atts, recordKey(accountID, id))
}

// terminalTransitGateway reads a deleted Transit Gateway still in its window.
func (s *Store) terminalTransitGateway(ctx context.Context, accountID, id string) (*TransitGatewayRecord, error) {
	rec, _, err := getJSON[TransitGatewayRecord](ctx, s.term, recordKey(accountID, id))
	if err != nil || rec == nil || !s.visible(rec.TerminalAt, transitGatewayWindow) {
		return nil, err
	}
	return rec, nil
}

// terminalAttachment reads a deleted or failed attachment still in its window.
func (s *Store) terminalAttachment(ctx context.Context, accountID, id string) (*AttachmentRecord, error) {
	rec, _, err := getJSON[AttachmentRecord](ctx, s.term, recordKey(accountID, id))
	if err != nil || rec == nil || !s.visible(rec.TerminalAt, attachmentWindow) {
		return nil, err
	}
	return rec, nil
}

func (s *Store) visible(terminalAt time.Time, window time.Duration) bool {
	return s.now().Sub(terminalAt) < window
}

// touchAt rewrites a key's stored bytes unchanged at rev, so a writer that
// read rev loses. A key absent or past rev is errConflict.
func touchAt(ctx context.Context, kv jetstream.KeyValue, key string, rev uint64) error {
	entry, err := kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return errConflict
		}
		return fmt.Errorf("read %s: %w", key, err)
	}
	if entry.Revision() != rev {
		return errConflict
	}
	if _, err := kv.Update(ctx, key, entry.Value(), rev); err != nil {
		if kvstore.IsRevisionConflict(err) {
			return errConflict
		}
		return fmt.Errorf("rewrite %s: %w", key, err)
	}
	return nil
}

// takeClaim takes key in kv for mine.Owner, refreshing its own claim or taking
// over one that stale judges. It returns the revision written, which the owner
// rewrites after its record write so a takeover during a pause is seen.
func (s *Store) takeClaim(ctx context.Context, kv jetstream.KeyValue, key string, mine claim, stale func(context.Context, *claim) (bool, error)) (uint64, error) {
	for range moveAttempts {
		rev, err := writeJSON(ctx, kv, key, mine, 0)
		if !errors.Is(err, errConflict) {
			return rev, err
		}
		held, heldRev, err := getJSON[claim](ctx, kv, key)
		if err != nil {
			return 0, err
		}
		if held == nil {
			continue
		}
		if held.Owner != mine.Owner {
			ok, err := stale(ctx, held)
			if err != nil {
				return 0, err
			}
			if !ok {
				return 0, errClaimHeld
			}
		}
		rev, err = writeJSON(ctx, kv, key, mine, heldRev)
		if !errors.Is(err, errConflict) {
			return rev, err
		}
	}
	return 0, fmt.Errorf("take %s: %w", key, errConflict)
}

func (s *Store) attachmentClaim(att *AttachmentRecord) claim {
	return claim{Owner: att.ID, AccountID: att.AccountID, ClaimedAt: s.now()}
}

// attachmentHolderStale judges a pair. or link. claim: its holder is gone past
// the grace, or has ended. A live holder keeps it; AWS counts a deleting
// attachment as non-deleted.
func (s *Store) attachmentHolderStale(ctx context.Context, held *claim) (bool, error) {
	holder, _, err := s.attachment(ctx, held.AccountID, held.Owner)
	if err != nil {
		return false, err
	}
	if holder == nil {
		return s.now().Sub(held.ClaimedAt) >= claimGrace, nil
	}
	return !holder.isLive(), nil
}

// transitGatewayHolderStale judges a quota slot: its holder is gone past the
// grace, or is deleted.
func (s *Store) transitGatewayHolderStale(ctx context.Context, held *claim) (bool, error) {
	holder, _, err := s.transitGateway(ctx, held.AccountID, held.Owner)
	if err != nil {
		return false, err
	}
	if holder == nil {
		return s.now().Sub(held.ClaimedAt) >= claimGrace, nil
	}
	return holder.State == StateDeleted, nil
}

// takeQuotaSlot takes one of the account's accountLimit slots.
func (s *Store) takeQuotaSlot(ctx context.Context, rec *TransitGatewayRecord) (string, uint64, error) {
	mine := claim{Owner: rec.ID, AccountID: rec.AccountID, ClaimedAt: s.now()}
	for n := range accountLimit {
		key := slotKey(rec.AccountID, n)
		rev, err := s.takeClaim(ctx, s.tgws, key, mine, s.transitGatewayHolderStale)
		if err == nil {
			return key, rev, nil
		}
		if !errors.Is(err, errClaimHeld) {
			return "", 0, err
		}
	}
	return "", 0, errClaimHeld
}

// releaseQuotaSlot releases whichever of the account's slots names id.
func (s *Store) releaseQuotaSlot(ctx context.Context, accountID, id string) error {
	for n := range accountLimit {
		if err := s.releaseClaim(ctx, s.tgws, slotKey(accountID, n), id); err != nil {
			return err
		}
	}
	return nil
}

// takeLinkSlot claims a /30 for the attachment, probing from a slot derived
// from its ID so a retried pass finds its own claim first.
func (s *Store) takeLinkSlot(ctx context.Context, att *AttachmentRecord) (string, string, uint64, error) {
	start := linkStart(att.ID, s.slots)
	mine := s.attachmentClaim(att)
	for i := range s.slots {
		n := (start + i) % s.slots
		rev, err := s.takeClaim(ctx, s.atts, linkKey(n), mine, s.attachmentHolderStale)
		if err == nil {
			return linkPrefix(n).String(), linkKey(n), rev, nil
		}
		if !errors.Is(err, errClaimHeld) {
			return "", "", 0, err
		}
	}
	return "", "", 0, errLinkSpaceExhausted
}

func linkStart(id string, slots int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return int(h.Sum32() % uint32(slots)) //nolint:gosec // slots is a small positive constant
}

// releaseClaim deletes key at its revision while it still names id.
func (s *Store) releaseClaim(ctx context.Context, kv jetstream.KeyValue, key, id string) error {
	held, rev, err := getJSON[claim](ctx, kv, key)
	if err != nil || held == nil || held.Owner != id {
		return err
	}
	if err := deleteAt(ctx, kv, key, rev); err != nil && !errors.Is(err, errConflict) {
		return err
	}
	return nil
}

// releaseLinkClaim releases the link slot a record carries.
func (s *Store) releaseLinkClaim(ctx context.Context, rec *AttachmentRecord) error {
	n, ok := linkSlot(rec.LinkPrefix)
	if !ok {
		return nil
	}
	return s.releaseClaim(ctx, s.atts, linkKey(n), rec.ID)
}

// attachmentIndexKeys are every index key a live attachment holds.
func attachmentIndexKeys(rec *AttachmentRecord) []string {
	keys := []string{
		acctKey(rec.AccountID, rec.ID),
		tgwIndexKey(rec.TransitGatewayID, rec.ID),
		vpcIndexKey(rec.VpcID, rec.ID),
	}
	for _, sub := range rec.SubnetIDs {
		keys = append(keys, subnetIndexKey(sub, rec.ID))
	}
	return keys
}

// writeAttachmentIndexes writes rec's index keys and returns their revisions.
func (s *Store) writeAttachmentIndexes(ctx context.Context, rec *AttachmentRecord) (map[string]uint64, error) {
	entry := indexEntry{AccountID: rec.AccountID, CreatedAt: rec.CreatedAt}
	revs := map[string]uint64{}
	for _, k := range attachmentIndexKeys(rec) {
		rev, err := putJSON(ctx, s.atts, k, entry)
		if err != nil {
			return nil, err
		}
		revs[k] = rev
	}
	return revs, nil
}

func (s *Store) deleteAttachmentIndexes(ctx context.Context, rec *AttachmentRecord) error {
	for _, k := range attachmentIndexKeys(rec) {
		if err := deleteKey(ctx, s.atts, k); err != nil {
			return err
		}
	}
	return nil
}

// releaseAttachment releases every claim naming rec and deletes its index keys.
func (s *Store) releaseAttachment(ctx context.Context, rec *AttachmentRecord) error {
	if err := s.releaseClaim(ctx, s.atts, pairKey(rec.TransitGatewayID, rec.VpcID), rec.ID); err != nil {
		return err
	}
	if err := s.releaseLinkClaim(ctx, rec); err != nil {
		return err
	}
	return s.deleteAttachmentIndexes(ctx, rec)
}

// writeTerminal stores rec and its account key in the terminal bucket.
func (s *Store) writeTerminal(ctx context.Context, accountID, id string, rec any, createdAt time.Time) error {
	if _, err := putJSON(ctx, s.term, recordKey(accountID, id), rec); err != nil {
		return err
	}
	_, err := putJSON(ctx, s.term, acctKey(accountID, id), indexEntry{AccountID: accountID, CreatedAt: createdAt})
	return err
}

// moveAttachmentTerminal writes the live record in its terminal state at rev,
// then copies it to the terminal bucket and releases its claims and keys. The
// first write is the fence; every later step is idempotent.
func (s *Store) moveAttachmentTerminal(ctx context.Context, rec *AttachmentRecord, rev uint64) error {
	newRev, err := writeJSON(ctx, s.atts, recordKey(rec.AccountID, rec.ID), rec, rev)
	if err != nil {
		return err
	}
	return s.finishAttachmentMove(ctx, rec, newRev)
}

// finishAttachmentMove completes a move whose terminal state is written.
func (s *Store) finishAttachmentMove(ctx context.Context, rec *AttachmentRecord, rev uint64) error {
	if err := s.writeTerminal(ctx, rec.AccountID, rec.ID, rec, rec.CreatedAt); err != nil {
		return err
	}
	if err := deleteAt(ctx, s.atts, recordKey(rec.AccountID, rec.ID), rev); err != nil {
		return err
	}
	return s.releaseAttachment(ctx, rec)
}

// moveTransitGatewayTerminal is moveAttachmentTerminal for a Transit Gateway.
func (s *Store) moveTransitGatewayTerminal(ctx context.Context, rec *TransitGatewayRecord, rev uint64) error {
	newRev, err := writeJSON(ctx, s.tgws, recordKey(rec.AccountID, rec.ID), rec, rev)
	if err != nil {
		return err
	}
	return s.finishTransitGatewayMove(ctx, rec, newRev)
}

func (s *Store) finishTransitGatewayMove(ctx context.Context, rec *TransitGatewayRecord, rev uint64) error {
	if err := s.writeTerminal(ctx, rec.AccountID, rec.ID, rec, rec.CreatedAt); err != nil {
		return err
	}
	if err := deleteAt(ctx, s.tgws, recordKey(rec.AccountID, rec.ID), rev); err != nil {
		return err
	}
	if err := deleteKey(ctx, s.tgws, acctKey(rec.AccountID, rec.ID)); err != nil {
		return err
	}
	return s.releaseQuotaSlot(ctx, rec.AccountID, rec.ID)
}

// liveAttachmentsUnder returns the IDs of the live attachments, and the young
// orphan keys, in an index prefix such as "tgw.<id>.". Either blocks a delete.
func (s *Store) liveAttachmentsUnder(ctx context.Context, prefix string) ([]string, error) {
	ids, err := listIndex(ctx, s.atts, prefix)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, id := range ids {
		entry, _, err := getJSON[indexEntry](ctx, s.atts, prefix+id)
		if err != nil {
			return nil, err
		}
		if entry == nil {
			continue
		}
		rec, _, err := s.attachment(ctx, entry.AccountID, id)
		if err != nil {
			return nil, err
		}
		if (rec != nil && rec.isLive()) || (rec == nil && s.now().Sub(entry.CreatedAt) < claimGrace) {
			out = append(out, id)
		}
	}
	return out, nil
}

// removeTransitGateway undoes a create: its record, its account key and its slot.
func (s *Store) removeTransitGateway(ctx context.Context, rec *TransitGatewayRecord) error {
	if err := deleteKey(ctx, s.tgws, recordKey(rec.AccountID, rec.ID)); err != nil {
		return err
	}
	if err := deleteKey(ctx, s.tgws, acctKey(rec.AccountID, rec.ID)); err != nil {
		return err
	}
	return s.releaseQuotaSlot(ctx, rec.AccountID, rec.ID)
}

func exists[T any](ctx context.Context, kv jetstream.KeyValue, accountID, id string) (bool, error) {
	rec, _, err := getJSON[T](ctx, kv, recordKey(accountID, id))
	return rec != nil, err
}
