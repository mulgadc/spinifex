package route53

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go/jetstream"
)

// KVBucketRoute53 holds every hosted zone, its record sets and its changes.
const (
	KVBucketRoute53        = "spinifex-route53"
	KVBucketRoute53Version = 1
	// KVBucketRoute53Realize elects the one node that publishes zones.
	KVBucketRoute53Realize = "spinifex-route53-realize"
	// KVBucketRoute53Verify elects the one node that checks delegations.
	KVBucketRoute53Verify = "spinifex-route53-verify"

	keyPrefixZone     = "zone."
	keyPrefixRRSets   = "rrsets."
	keyPrefixChange   = "change."
	keyPrefixCallerRf = "ref."
	keyPrefixPublish  = "published."
	casAttempts       = 20
)

// Activation states. A zone is served by northstar only while ACTIVE.
const (
	StatePendingNameservers = "PENDING_NAMESERVERS"
	StateActive             = "ACTIVE"
	StateNameserversLost    = "NAMESERVERS_LOST"
	// StateSuspended is an operator's deactivation; only the operator lifts it.
	StateSuspended = "SUSPENDED"
)

// Change statuses, as GetChange reports them.
const (
	ChangeStatusPending = "PENDING"
	ChangeStatusInSync  = "INSYNC"
)

// ZoneRecord is a hosted zone's identity and configuration. Its record sets
// live in a separate RRSetsDoc so a change batch is one CAS on one document.
type ZoneRecord struct {
	ID              string            `json:"id"`
	AccountID       string            `json:"account_id"`
	Name            string            `json:"name"`
	CallerReference string            `json:"caller_reference"`
	Comment         string            `json:"comment,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	Nameservers     []string          `json:"nameservers"`
	Tags            map[string]string `json:"tags,omitempty"`

	State       string    `json:"state"`
	StateReason string    `json:"state_reason,omitempty"`
	CheckedAt   time.Time `json:"checked_at"`
	// Delegation is the NS set the parent published at the last check, and
	// DelegationSource the parent zone or server that answered.
	Delegation       []string  `json:"delegation,omitempty"`
	DelegationSource string    `json:"delegation_source,omitempty"`
	FailingSince     time.Time `json:"failing_since,omitzero"`
	// Override is set when an operator activated or suspended the zone; the
	// verifier leaves such a zone alone.
	Override       string `json:"override,omitempty"`
	OverrideReason string `json:"override_reason,omitempty"`
}

// RRSetsDoc is a zone's complete record-set document. Version increases by one
// on every committed change batch and is what a realized zone is stamped with.
type RRSetsDoc struct {
	ZoneID    string    `json:"zone_id"`
	Version   uint64    `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
	Sets      []RRSet   `json:"sets"`
}

// RRSet is one simple-routing record set. Name is normalized.
type RRSet struct {
	Name   string   `json:"name"`
	Type   string   `json:"type"`
	TTL    int64    `json:"ttl"`
	Values []string `json:"values"`
}

// ChangeRecord tracks one submitted change for GetChange.
type ChangeRecord struct {
	ID          string    `json:"id"`
	AccountID   string    `json:"account_id"`
	ZoneID      string    `json:"zone_id"`
	Version     uint64    `json:"version"`
	Status      string    `json:"status"`
	Comment     string    `json:"comment,omitempty"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// PublishedZone records what the realizer last wrote to northstar for a zone
// name. It is how a withdrawn zone is found, since its own records may be gone.
type PublishedZone struct {
	Apex      string `json:"apex"`
	AccountID string `json:"account_id"`
	ZoneID    string `json:"zone_id"`
	Version   uint64 `json:"version"`
	Digest    string `json:"digest"`
	InSync    bool   `json:"in_sync"`
}

type callerRef struct {
	ZoneID string `json:"zone_id"`
}

// Store reads and writes the route53 bucket. Keys are account-scoped, so an
// owner lookup never reads another account's records.
type Store struct {
	bucket  *kvstore.Bucket
	zones   *kvstore.Store[ZoneRecord]
	rrsets  *kvstore.Store[RRSetsDoc]
	changes *kvstore.Store[ChangeRecord]
	refs    *kvstore.Store[callerRef]
	publish *kvstore.Store[PublishedZone]
}

// NewStore returns a Store over js. The bucket is created on first use.
func NewStore(js jetstream.JetStream) *Store {
	b := kvstore.NewBucket(js, kvstore.Config{
		Name:        KVBucketRoute53,
		History:     KVBucketRoute53Version,
		Description: "Route 53 hosted zones, record sets and changes",
		Missing:     "route53 store has no JetStream connection",
		Attempts:    casAttempts,
		Exhausted: func(key string, attempts int) error {
			return fmt.Errorf("route53 store: exceeded %d CAS attempts updating %s", attempts, key)
		},
	})
	return &Store{
		bucket:  b,
		zones:   kvstore.On[ZoneRecord](b),
		rrsets:  kvstore.On[RRSetsDoc](b),
		changes: kvstore.On[ChangeRecord](b),
		refs:    kvstore.On[callerRef](b),
		publish: kvstore.On[PublishedZone](b),
	}
}

func zoneKey(account, id string) string   { return keyPrefixZone + kvutil.AccountKey(account, id) }
func rrsetsKey(account, id string) string { return keyPrefixRRSets + kvutil.AccountKey(account, id) }
func changeKey(account, id string) string { return keyPrefixChange + kvutil.AccountKey(account, id) }
func refKey(account, ref string) string {
	return keyPrefixCallerRf + kvutil.AccountKey(account, refDigest(ref))
}

// GetZone returns an account's zone, or kvstore.ErrNotFound.
func (s *Store) GetZone(ctx context.Context, account, id string) (*ZoneRecord, error) {
	z, _, err := s.zones.Get(ctx, zoneKey(account, id))
	return z, err
}

// ListAccountZones returns every zone the account owns.
func (s *Store) ListAccountZones(ctx context.Context, account string) ([]ZoneRecord, error) {
	return s.zones.List(ctx, keyPrefixZone+account+".")
}

// ListAllZones returns every zone in every account, for the realizer and the
// verifier, which act across accounts.
func (s *Store) ListAllZones(ctx context.Context) ([]ZoneRecord, error) {
	return s.zones.List(ctx, keyPrefixZone)
}

// MutateZone applies fn to a zone under CAS.
func (s *Store) MutateZone(ctx context.Context, account, id string, fn func(*ZoneRecord) (bool, error)) error {
	return s.zones.Mutate(ctx, zoneKey(account, id), fn)
}

// GetRRSets returns a zone's record sets and the revision to CAS against.
func (s *Store) GetRRSets(ctx context.Context, account, id string) (*RRSetsDoc, uint64, error) {
	return s.rrsets.Get(ctx, rrsetsKey(account, id))
}

// CommitRRSets writes doc if the document is still at rev.
func (s *Store) CommitRRSets(ctx context.Context, account string, doc *RRSetsDoc, rev uint64) error {
	_, err := s.rrsets.CompareAndSet(ctx, rrsetsKey(account, doc.ZoneID), doc, rev)
	return err
}

// GetChange returns an account's change, or kvstore.ErrNotFound.
func (s *Store) GetChange(ctx context.Context, account, id string) (*ChangeRecord, error) {
	c, _, err := s.changes.Get(ctx, changeKey(account, id))
	return c, err
}

// PutChange stores a new change.
func (s *Store) PutChange(ctx context.Context, c *ChangeRecord) error {
	_, err := s.changes.Create(ctx, changeKey(c.AccountID, c.ID), c)
	return err
}

// ListZoneChanges returns every change recorded for one zone.
func (s *Store) ListZoneChanges(ctx context.Context, account, zoneID string) ([]ChangeRecord, error) {
	all, err := s.changes.List(ctx, keyPrefixChange+account+".")
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, c := range all {
		if c.ZoneID == zoneID {
			out = append(out, c)
		}
	}
	return out, nil
}

// MarkChangesInSync moves every PENDING change for a zone at or below version
// to INSYNC.
func (s *Store) MarkChangesInSync(ctx context.Context, account, zoneID string, version uint64) error {
	changes, err := s.ListZoneChanges(ctx, account, zoneID)
	if err != nil {
		return err
	}
	for _, c := range changes {
		if c.Status != ChangeStatusPending || c.Version > version {
			continue
		}
		err := s.changes.Mutate(ctx, changeKey(account, c.ID), func(rec *ChangeRecord) (bool, error) {
			if rec.Status == ChangeStatusInSync {
				return false, nil
			}
			rec.Status = ChangeStatusInSync
			return true, nil
		})
		if err != nil && !errors.Is(err, kvstore.ErrNotFound) {
			return err
		}
	}
	return nil
}

// ClaimCallerReference records that ref created zoneID. It returns the zone
// already holding ref when the claim loses.
func (s *Store) ClaimCallerReference(ctx context.Context, account, ref, zoneID string) (existing string, err error) {
	if _, err := s.refs.Create(ctx, refKey(account, ref), &callerRef{ZoneID: zoneID}); err != nil {
		if !errors.Is(err, kvstore.ErrExists) {
			return "", err
		}
		held, _, getErr := s.refs.Get(ctx, refKey(account, ref))
		if getErr != nil {
			return "", getErr
		}
		return held.ZoneID, nil
	}
	return "", nil
}

// CreateZone writes a new zone and its initial record sets.
func (s *Store) CreateZone(ctx context.Context, z *ZoneRecord, doc *RRSetsDoc) error {
	if _, err := s.rrsets.Create(ctx, rrsetsKey(z.AccountID, z.ID), doc); err != nil {
		return err
	}
	if _, err := s.zones.Create(ctx, zoneKey(z.AccountID, z.ID), z); err != nil {
		_ = s.bucket.Delete(ctx, rrsetsKey(z.AccountID, z.ID))
		return err
	}
	return nil
}

// DeleteZone removes a zone whose record sets are still at rev, returning
// kvstore.ErrConflict if a change landed since they were read. The record-set
// document goes first, so a concurrent change batch cannot commit into a zone
// that is being deleted; the zone's earlier changes go with it.
func (s *Store) DeleteZone(ctx context.Context, z *ZoneRecord, rev uint64) error {
	if err := s.bucket.CompareAndDelete(ctx, rrsetsKey(z.AccountID, z.ID), rev); err != nil {
		return err
	}
	if err := s.bucket.Delete(ctx, zoneKey(z.AccountID, z.ID)); err != nil {
		return err
	}
	if err := s.bucket.Delete(ctx, refKey(z.AccountID, z.CallerReference)); err != nil {
		return err
	}
	return s.PruneChanges(ctx, z.AccountID, z.ID, time.Time{}, true)
}

// PruneChanges deletes a zone's INSYNC changes submitted before cutoff, or all
// of them when all is set. A PENDING change is kept until it is in sync.
func (s *Store) PruneChanges(ctx context.Context, account, zoneID string, cutoff time.Time, all bool) error {
	changes, err := s.ListZoneChanges(ctx, account, zoneID)
	if err != nil {
		return err
	}
	for _, c := range changes {
		if !all && (c.Status != ChangeStatusInSync || !c.SubmittedAt.Before(cutoff)) {
			continue
		}
		if err := s.bucket.Delete(ctx, changeKey(account, c.ID)); err != nil {
			return err
		}
	}
	return nil
}

// ReleaseCallerReference drops a claim whose zone was never created, so the
// caller can retry with the same reference.
func (s *Store) ReleaseCallerReference(ctx context.Context, account, ref string) error {
	return s.bucket.Delete(ctx, refKey(account, ref))
}

// ListAllChanges returns every change in every account.
func (s *Store) ListAllChanges(ctx context.Context) ([]ChangeRecord, error) {
	return s.changes.List(ctx, keyPrefixChange)
}

// DeleteChange removes one change.
func (s *Store) DeleteChange(ctx context.Context, account, id string) error {
	return s.bucket.Delete(ctx, changeKey(account, id))
}

// ListPublished returns the published zones keyed by apex.
func (s *Store) ListPublished(ctx context.Context) (map[string]PublishedZone, error) {
	all, err := s.publish.List(ctx, keyPrefixPublish)
	if err != nil {
		return nil, err
	}
	out := make(map[string]PublishedZone, len(all))
	for _, p := range all {
		out[p.Apex] = p
	}
	return out, nil
}

// SetPublished records what was written for p.Apex.
func (s *Store) SetPublished(ctx context.Context, p *PublishedZone) error {
	return s.publish.Set(ctx, keyPrefixPublish+p.Apex, p)
}

// DeletePublished forgets a withdrawn zone.
func (s *Store) DeletePublished(ctx context.Context, apex string) error {
	return s.bucket.Delete(ctx, keyPrefixPublish+apex)
}
