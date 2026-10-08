package access

import (
	"context"
	"errors"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Owner is the sole writer of EKS AccessEntry records. Every method takes
// the account's EKS bucket, which the caller has already opened.
type Owner struct {
	region string
}

// New returns an Owner minting ARNs in region.
func New(region string) *Owner {
	return &Owner{region: region}
}

// ErrExists is returned by Create when a record already exists for the
// principal.
var ErrExists = errors.New("eks: access entry already exists")

// Spec is a validated CreateAccessEntry request: everything a record stores
// except its identity and timestamps.
type Spec struct {
	Cluster      string
	PrincipalARN string
	Username     string
	Groups       []string
	Type         string
	Tags         map[string]string
}

// Create stores a new AccessEntry record. Returns ErrExists if one already
// exists for the principal; the existing record is left untouched.
func (o *Owner) Create(ctx context.Context, kv jetstream.KeyValue, accountID string, spec Spec) (*Record, error) {
	if _, err := Get(ctx, kv, spec.Cluster, spec.PrincipalARN); err == nil {
		return nil, ErrExists
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	rec := newRecord(o.region, accountID, spec.Cluster, spec.PrincipalARN,
		spec.Username, spec.Groups, spec.Type, spec.Tags, time.Now().UTC())
	if err := put(ctx, kv, rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// SeedCreatorAdmin unconditionally writes the system:masters AccessEntry for
// a cluster's creator at launch, overwriting any existing record for the
// principal. That overwrite is a known gap, not fixed here.
func (o *Owner) SeedCreatorAdmin(ctx context.Context, kv jetstream.KeyValue, accountID, cluster, principalARN string) error {
	rec := newRecord(o.region, accountID, cluster, principalARN, "", []string{"system:masters"},
		EntryTypeStandard, nil, time.Now().UTC())
	return put(ctx, kv, rec)
}

// Get reads one entry. Returns ErrNotFound if absent.
func (o *Owner) Get(ctx context.Context, kv jetstream.KeyValue, cluster, principalARN string) (*Record, error) {
	return Get(ctx, kv, cluster, principalARN)
}

// List returns a cluster's access entries sorted by principal ARN, filtered
// to entries with policyFilter associated when policyFilter is non-empty.
func (o *Owner) List(ctx context.Context, kv jetstream.KeyValue, cluster, policyFilter string) ([]*Record, error) {
	recs, err := List(ctx, kv, cluster)
	if err != nil {
		return nil, err
	}
	if policyFilter == "" {
		return recs, nil
	}
	out := make([]*Record, 0, len(recs))
	for _, rec := range recs {
		if HasAssociatedPolicy(rec, policyFilter) {
			out = append(out, rec)
		}
	}
	return out, nil
}

// Update changes a record's username and/or Kubernetes groups; groups only
// changes when groupsSet is true. ModifiedAt always advances, even on a
// no-op update.
func (o *Owner) Update(ctx context.Context, kv jetstream.KeyValue, cluster, principalARN, username string, groups []string, groupsSet bool) (*Record, error) {
	now := time.Now().UTC()
	return casUpdate(ctx, kv, cluster, principalARN, func(r *Record) bool {
		if groupsSet {
			r.KubernetesGroups = groups
		}
		if username != "" {
			r.KubernetesUsername = username
		}
		r.ModifiedAt = now
		return true
	})
}

// Associate upserts a managed access-policy association, preserving the
// original AssociatedAt on re-associate.
func (o *Owner) Associate(ctx context.Context, kv jetstream.KeyValue, cluster, principalARN, policyARN string, scope Scope) (AssociatedPolicy, error) {
	now := time.Now().UTC()
	var assoc AssociatedPolicy
	_, err := casUpdate(ctx, kv, cluster, principalARN, func(r *Record) bool {
		assoc = AssociatedPolicy{PolicyARN: policyARN, AccessScope: scope, AssociatedAt: now, ModifiedAt: now}
		for i := range r.AssociatedPolicies {
			if r.AssociatedPolicies[i].PolicyARN == policyARN {
				assoc.AssociatedAt = r.AssociatedPolicies[i].AssociatedAt
				r.AssociatedPolicies[i] = assoc
				r.ModifiedAt = now
				return true
			}
		}
		r.AssociatedPolicies = append(r.AssociatedPolicies, assoc)
		r.ModifiedAt = now
		return true
	})
	if err != nil {
		return AssociatedPolicy{}, err
	}
	return assoc, nil
}

// Disassociate removes a policy association; an association that does not
// exist is a no-op that performs no write.
func (o *Owner) Disassociate(ctx context.Context, kv jetstream.KeyValue, cluster, principalARN, policyARN string) error {
	now := time.Now().UTC()
	_, err := casUpdate(ctx, kv, cluster, principalARN, func(r *Record) bool {
		for i := range r.AssociatedPolicies {
			if r.AssociatedPolicies[i].PolicyARN == policyARN {
				r.AssociatedPolicies = append(r.AssociatedPolicies[:i], r.AssociatedPolicies[i+1:]...)
				r.ModifiedAt = now
				return true
			}
		}
		return false
	})
	return err
}

// Delete removes one entry; returns ErrNotFound if absent.
func (o *Owner) Delete(ctx context.Context, kv jetstream.KeyValue, cluster, principalARN string) error {
	return deleteRecord(ctx, kv, cluster, principalARN)
}
