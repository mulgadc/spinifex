package elbv2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	resourcearn "github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// realizationOVN marks a load balancer whose data path OVN builds.
const realizationOVN = "ovn"

// fixtureOwnerRevision is the owner revision a new root is created and sealed at.
const fixtureOwnerRevision = 1

var (
	// errFixtureNameBusy reports an account/name held by another owner that has
	// not expired, or by a sealed root of another load balancer.
	errFixtureNameBusy = errors.New("elbv2: load balancer name is owned by another create")
	// errFixtureOwnerLost reports that the claim no longer holds the caller's
	// pending root, so the caller may not seal or invalidate it.
	errFixtureOwnerLost = errors.New("elbv2: load balancer name claim is no longer owned")
	// errFixtureSealed reports an attempt to invalidate a sealed root.
	errFixtureSealed = errors.New("elbv2: load balancer root is sealed")
	// errFixtureClaimMalformed reports a claim value that is not a complete
	// fixture owner. The store never takes over or hides such a value.
	errFixtureClaimMalformed = errors.New("elbv2: load balancer name claim is malformed")
	// errFixtureSealRejected reports a seal write known not to be stored, with
	// the claim re-read still pending at the caller's revision.
	errFixtureSealRejected = errors.New("elbv2: load balancer seal was rejected")
)

// fixtureOwner is the value at the account/name claim key. OwnerID and
// ClaimedAt keep the appliance claim format; the root is readable only after
// the claim is sealed at the same owner revision.
type fixtureOwner struct {
	OwnerID       string              `json:"ownerId"`
	ClaimedAt     time.Time           `json:"claimedAt"`
	OwnerRevision uint64              `json:"ownerRevision,omitempty"`
	Sealed        bool                `json:"sealed,omitempty"`
	Root          *LoadBalancerRecord `json:"root,omitempty"`
}

// fixtureClaim is a claim held by the caller: its key, the KV revision the
// caller wrote and the owner bytes at that revision. Seal uses those bytes, so
// a caller change to its root after Begin cannot reach the sealed root.
type fixtureClaim struct {
	Key      string
	Revision uint64
	pending  []byte
}

// snapshot decodes the persisted pending owner. Store logic reads the owner
// token, parents and root only from it.
func (c *fixtureClaim) snapshot() (fixtureOwner, error) {
	owner, err := decodeFixtureOwner(c.pending)
	if err != nil {
		return fixtureOwner{}, fmt.Errorf("pending claim %s: %w", c.Key, err)
	}
	return owner, nil
}

func newFixtureClaim(key string, revision uint64, raw []byte) (*fixtureClaim, error) {
	if _, err := decodeFixtureOwner(raw); err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	return &fixtureClaim{Key: key, Revision: revision, pending: raw}, nil
}

// fixtureOwnerStore keeps each OVN load balancer root in its account/name
// claim, outside the appliance lb. prefix.
type fixtureOwnerStore struct {
	kv       jetstream.KeyValue
	parents  *ec2vpc.Parents
	now      func() time.Time
	claimTTL time.Duration
}

func newFixtureOwnerStore(s *Store, parents *ec2vpc.Parents) *fixtureOwnerStore {
	return &fixtureOwnerStore{kv: s.kv, parents: parents, now: s.now, claimTTL: s.claimTTL}
}

func decodeFixtureOwner(raw []byte) (fixtureOwner, error) {
	var owner fixtureOwner
	if err := json.Unmarshal(raw, &owner); err != nil {
		return fixtureOwner{}, fmt.Errorf("%w: %w", errFixtureClaimMalformed, err)
	}
	if owner.OwnerID == "" || owner.ClaimedAt.IsZero() || owner.OwnerRevision != fixtureOwnerRevision ||
		owner.Root == nil || owner.Root.LoadBalancerID != owner.OwnerID {
		return fixtureOwner{}, errFixtureClaimMalformed
	}
	return owner, nil
}

// definiteKVConflict is true when the server refused a conditional write, so
// the write did not happen. Any other error leaves the outcome unknown.
func definiteKVConflict(err error) bool {
	return errors.Is(err, jetstream.ErrKeyExists) || errors.Is(err, jetstream.ErrKeyRevisionMismatch) ||
		kvstore.IsRevisionConflict(err)
}

// sealWriteRejected is true only for errors that prove the write was not
// stored: the client's payload check before publish, and the stream's
// message-size refusal. Every other error leaves the outcome unknown.
func sealWriteRejected(err error) bool {
	if errors.Is(err, nats.ErrMaxPayload) {
		return true
	}
	apiErr, ok := errors.AsType[*jetstream.APIError](err)
	return ok && apiErr.ErrorCode == jetstream.JSErrCodeStreamMessageExceedsMaximum
}

func (f *fixtureOwnerStore) read(ctx context.Context, key string) (jetstream.KeyValueEntry, *fixtureOwner, error) {
	entry, err := f.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("kv get %s: %w", key, err)
	}
	owner, err := decodeFixtureOwner(entry.Value())
	if err != nil {
		return nil, nil, fmt.Errorf("decode %s: %w", key, err)
	}
	return entry, &owner, nil
}

// Begin claims the root's account/name with a pending root. sealed is
// non-nil when a sealed root already holds the name, so the caller can compare
// it with the request. A pending claim of another owner blocks until it expires.
func (f *fixtureOwnerStore) Begin(ctx context.Context, root *LoadBalancerRecord) (claim *fixtureClaim, sealed *LoadBalancerRecord, err error) {
	if root == nil || root.LoadBalancerID == "" || root.Name == "" || root.AccountID == "" {
		return nil, nil, errors.New("elbv2: fixture root needs an ID, name and account")
	}
	key := LBNameKey(root.Name, root.AccountID)
	owner := fixtureOwner{OwnerID: root.LoadBalancerID, ClaimedAt: f.now(), OwnerRevision: fixtureOwnerRevision, Root: root}
	value, err := json.Marshal(owner)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal fixture owner %s: %w", key, err)
	}
	for range maxLBNameClaimRetries {
		rev, cerr := f.kv.Create(ctx, key, value)
		if cerr == nil {
			c, err := newFixtureClaim(key, rev, value)
			return c, nil, err
		}
		entry, cur, rerr := f.read(ctx, key)
		if rerr != nil {
			return nil, nil, rerr
		}
		if cur == nil {
			if definiteKVConflict(cerr) {
				continue
			}
			return nil, nil, fmt.Errorf("kv create %s: %w", key, cerr)
		}
		if cur.OwnerID == root.LoadBalancerID {
			if cur.Sealed {
				return nil, cur.Root, nil
			}
			c, err := newFixtureClaim(key, entry.Revision(), entry.Value())
			return c, nil, err
		}
		if cur.Sealed {
			return nil, cur.Root, nil
		}
		if f.now().Sub(cur.ClaimedAt) < f.claimTTL {
			return nil, nil, errFixtureNameBusy
		}
		rev, uerr := f.kv.Update(ctx, key, value, entry.Revision())
		if uerr == nil {
			c, err := newFixtureClaim(key, rev, value)
			return c, nil, err
		}
		if !definiteKVConflict(uerr) {
			if _, after, aerr := f.read(ctx, key); aerr == nil && after != nil && after.OwnerID == root.LoadBalancerID && !after.Sealed {
				continue
			}
			return nil, nil, fmt.Errorf("kv update %s: %w", key, uerr)
		}
	}
	return nil, nil, fmt.Errorf("elbv2: claim fixture name %s exhausted retries", key)
}

// Seal makes the pending root readable by a CAS of the exact claim revision.
// A failed write is resolved by re-reading the claim: the caller's own sealed
// root is success, a missing or replaced claim is errFixtureOwnerLost, and a
// known rejection with the claim still at its revision is errFixtureSealRejected.
func (f *fixtureOwnerStore) Seal(ctx context.Context, claim *fixtureClaim) (*LoadBalancerRecord, error) {
	sealed, err := claim.snapshot()
	if err != nil {
		return nil, err
	}
	if sealed.Sealed {
		return nil, fmt.Errorf("pending claim %s: %w", claim.Key, errFixtureClaimMalformed)
	}
	sealed.Sealed = true
	value, err := json.Marshal(sealed)
	if err != nil {
		return nil, fmt.Errorf("marshal fixture owner %s: %w", claim.Key, err)
	}
	_, uerr := f.kv.Update(ctx, claim.Key, value, claim.Revision)
	if uerr == nil {
		return sealed.Root, nil
	}
	entry, cur, rerr := f.read(ctx, claim.Key)
	if rerr != nil {
		return nil, errors.Join(fmt.Errorf("kv update %s: %w", claim.Key, uerr), rerr)
	}
	if cur != nil && cur.OwnerID == sealed.OwnerID && cur.OwnerRevision == sealed.OwnerRevision && cur.Sealed {
		return cur.Root, nil
	}
	if cur == nil || cur.OwnerID != sealed.OwnerID || definiteKVConflict(uerr) {
		return nil, errFixtureOwnerLost
	}
	if sealWriteRejected(uerr) && entry.Revision() == claim.Revision {
		return nil, fmt.Errorf("kv update %s: %w: %w", claim.Key, errFixtureSealRejected, uerr)
	}
	return nil, fmt.Errorf("kv update %s: %w", claim.Key, uerr)
}

// Invalidate deletes the caller's exact unsealed claim by revision. It never
// removes a replacement owner's claim or a sealed root, even when the caller
// supplies the sealed revision.
func (f *fixtureOwnerStore) Invalidate(ctx context.Context, claim *fixtureClaim) error {
	mine, err := claim.snapshot()
	if err != nil {
		return err
	}
	entry, cur, err := f.read(ctx, claim.Key)
	switch {
	case err != nil:
		return err
	case cur == nil:
		return nil
	case cur.OwnerID != mine.OwnerID:
		return errFixtureOwnerLost
	case cur.Sealed:
		return errFixtureSealed
	case entry.Revision() != claim.Revision:
		return errFixtureOwnerLost
	}
	derr := f.kv.Delete(ctx, claim.Key, jetstream.LastRevision(claim.Revision))
	if derr == nil {
		return nil
	}
	_, cur, rerr := f.read(ctx, claim.Key)
	if rerr != nil {
		return errors.Join(fmt.Errorf("kv delete %s: %w", claim.Key, derr), rerr)
	}
	switch {
	case cur == nil:
		return nil
	case cur.OwnerID != mine.OwnerID:
		return errFixtureOwnerLost
	case cur.Sealed:
		return errFixtureSealed
	}
	return fmt.Errorf("kv delete %s: %w", claim.Key, derr)
}

// GetSealed point-reads the sealed root for an account/name. A missing or
// pending claim reads as nil.
func (f *fixtureOwnerStore) GetSealed(ctx context.Context, name, accountID string) (*LoadBalancerRecord, error) {
	_, cur, err := f.read(ctx, LBNameKey(name, accountID))
	if err != nil || cur == nil || !cur.Sealed {
		return nil, err
	}
	return cur.Root, nil
}

// GetSealedByArn resolves a network load balancer ARN in the caller's region
// and account to its sealed root. The stored ARN must match exactly, and a
// name segment outside the load balancer name syntax is absent with no read.
func (f *fixtureOwnerStore) GetSealedByArn(ctx context.Context, arn, region, accountID string) (*LoadBalancerRecord, error) {
	parsed, ok := resourcearn.ParseELBv2(arn)
	if !ok || parsed.Kind != resourcearn.ELBv2LoadBalancer || parsed.Region != region || parsed.AccountID != accountID {
		return nil, nil
	}
	parts := strings.Split(parsed.Resource, "/")
	if len(parts) != 3 || parts[0] != resourcearn.ELBv2LBPathSegment(LoadBalancerTypeNetwork) || parts[2] == "" ||
		validateLBName(parts[1]) != nil {
		return nil, nil
	}
	root, err := f.GetSealed(ctx, parts[1], accountID)
	if err != nil || root == nil {
		return nil, err
	}
	if root.LoadBalancerArn != arn || root.LoadBalancerID != parts[2] || root.AccountID != accountID {
		return nil, nil
	}
	return root, nil
}
