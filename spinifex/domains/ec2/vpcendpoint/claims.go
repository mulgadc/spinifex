package vpcendpoint

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// claimLostError reports a table whose claim another endpoint holds and may
// keep, which AWS answers as the table already having the route.
type claimLostError struct{ table string }

func (e *claimLostError) Error() string { return "vpcendpoint: claim on " + e.table + " held" }

// takeClaim takes the S3 claim on table for rec, refreshing its own or taking
// over a stale one after fencing its holder.
func (s *Store) takeClaim(ctx context.Context, rec *EndpointRecord, table string) error {
	key := claimKey(table)
	mine := claim{VpceID: rec.ID, AccountID: rec.AccountID, ClaimedAt: s.now()}
	for range moveAttempts {
		if _, err := writeJSON(ctx, s.kv, key, mine, 0); !errors.Is(err, errConflict) {
			return err
		}
		held, rev, err := getJSON[claim](ctx, s.kv, key)
		if err != nil {
			return err
		}
		if held == nil {
			continue
		}
		if held.VpceID != rec.ID {
			stale, retry, err := s.fenceHolder(ctx, held, table)
			if err != nil {
				return err
			}
			if retry {
				continue
			}
			if !stale {
				return &claimLostError{table: table}
			}
		}
		if _, err := writeJSON(ctx, s.kv, key, mine, rev); !errors.Is(err, errConflict) {
			return err
		}
	}
	return fmt.Errorf("take %s: %w", key, errConflict)
}

// fenceHolder decides another endpoint's claim on table. A stale claim's
// holder is fenced at the revision read, so a stalled create or modify of it
// can never complete; retry asks for the claim to be re-read.
func (s *Store) fenceHolder(ctx context.Context, held *claim, table string) (stale, retry bool, err error) {
	holder, rev, err := s.load(ctx, held.AccountID, held.VpceID)
	if err != nil {
		return false, false, err
	}
	switch {
	case holder == nil || holder.State == StateDeleting:
		return true, false, nil
	case holder.State == StateAvailable && holder.lists(table):
		return false, false, nil
	case s.now().Sub(held.ClaimedAt) < claimGrace:
		return false, false, nil
	}
	err = s.fence(ctx, holder, rev)
	if errors.Is(err, errConflict) {
		return false, true, nil
	}
	return err == nil, false, err
}

// fence advances holder past the revision a stalled writer holds: a pending
// record is removed with its index key, and an available one is rewritten.
func (s *Store) fence(ctx context.Context, holder *EndpointRecord, rev uint64) error {
	key := recordKey(holder.AccountID, holder.ID)
	if holder.State == StatePending {
		if err := deleteAt(ctx, s.kv, key, rev); err != nil {
			return err
		}
		return deleteKey(ctx, s.kv, vpcKey(holder.VpcID, holder.ID))
	}
	_, err := writeJSON(ctx, s.kv, key, holder, rev)
	return err
}

// releaseClaim deletes table's claim at its revision while it names id.
func (s *Store) releaseClaim(ctx context.Context, table, id string) error {
	key := claimKey(table)
	held, rev, err := getJSON[claim](ctx, s.kv, key)
	if err != nil || held == nil || held.VpceID != id {
		return err
	}
	if err := deleteAt(ctx, s.kv, key, rev); err != nil && !errors.Is(err, errConflict) {
		return err
	}
	return nil
}

func (s *Store) releaseClaims(ctx context.Context, tables []string, id string) error {
	var errs []error
	for _, t := range tables {
		if err := s.releaseClaim(ctx, t, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// create writes rec in the publication order: index key, pending record,
// each claim, then available at the record's revision. A lost claim, or a
// fence by a taker, undoes the create and names the table it lost.
func (s *Store) create(ctx context.Context, rec *EndpointRecord) error {
	if err := putJSON(ctx, s.kv, vpcKey(rec.VpcID, rec.ID), indexEntry{AccountID: rec.AccountID, CreatedAt: rec.CreatedAt}); err != nil {
		return err
	}
	rec.State = StatePending
	rev, err := writeJSON(ctx, s.kv, recordKey(rec.AccountID, rec.ID), rec, 0)
	if err != nil {
		return errors.Join(err, deleteKey(ctx, s.kv, vpcKey(rec.VpcID, rec.ID)))
	}
	for i, t := range rec.RouteTableIds {
		if err := s.takeClaim(ctx, rec, t); err != nil {
			return errors.Join(err, s.abandon(ctx, rec, rev, rec.RouteTableIds[:i]))
		}
	}
	rec.State = StateAvailable
	if _, err := writeJSON(ctx, s.kv, recordKey(rec.AccountID, rec.ID), rec, rev); err != nil {
		if errors.Is(err, errConflict) {
			err = &claimLostError{table: s.lostTable(ctx, rec)}
		}
		return errors.Join(err, s.abandon(ctx, rec, rev, rec.RouteTableIds))
	}
	return nil
}

// abandon removes a create that did not complete: its pending record if no
// taker removed it first, its index key, and the claims it took.
func (s *Store) abandon(ctx context.Context, rec *EndpointRecord, rev uint64, taken []string) error {
	err := deleteAt(ctx, s.kv, recordKey(rec.AccountID, rec.ID), rev)
	if errors.Is(err, errConflict) {
		err = nil
	}
	return errors.Join(err,
		deleteKey(ctx, s.kv, vpcKey(rec.VpcID, rec.ID)),
		s.releaseClaims(ctx, taken, rec.ID))
}

// lostTable names the first of rec's tables whose claim no longer names it.
func (s *Store) lostTable(ctx context.Context, rec *EndpointRecord) string {
	for _, t := range rec.RouteTableIds {
		c, _, err := getJSON[claim](ctx, s.kv, claimKey(t))
		if err == nil && (c == nil || c.VpceID != rec.ID) {
			return t
		}
	}
	if len(rec.RouteTableIds) > 0 {
		return rec.RouteTableIds[0]
	}
	return ""
}

// errAbsent reports an endpoint with no available record.
var errAbsent = errors.New("vpcendpoint: endpoint absent")

// errNotAssociated reports a removal of a table the endpoint does not list.
var errNotAssociated = errors.New("vpcendpoint: table not associated")

// modification is one accepted ModifyVpcEndpoint, applied to the record read.
type modification struct {
	add, remove []string
	policy      *string // nil leaves the policy; "" resets it
	dns         *string // nil leaves the DNS record IP type
}

// modify applies m in the publication order: claims for added tables, the
// record at its revision, then the release of removed tables' claims.
func (s *Store) modify(ctx context.Context, accountID, id string, m modification) (*EndpointRecord, error) {
	var taken []string
	for range moveAttempts {
		rec, rev, err := s.load(ctx, accountID, id)
		if err != nil {
			return nil, errors.Join(err, s.releaseClaims(ctx, taken, id))
		}
		if rec == nil || rec.State != StateAvailable {
			return nil, errors.Join(errAbsent, s.releaseClaims(ctx, taken, id))
		}
		for _, t := range m.remove {
			if !rec.lists(t) {
				return nil, errors.Join(errNotAssociated, s.releaseClaims(ctx, unlisted(taken, rec), id))
			}
		}
		for _, t := range m.add {
			if err := s.takeClaim(ctx, rec, t); err != nil {
				return nil, errors.Join(err, s.releaseClaims(ctx, unlisted(taken, rec), id))
			}
			if !rec.lists(t) && !slices.Contains(taken, t) {
				taken = append(taken, t)
			}
		}
		tables := slices.Concat(rec.RouteTableIds, m.add)
		tables = slices.DeleteFunc(tables, func(t string) bool { return slices.Contains(m.remove, t) })
		slices.Sort(tables)
		rec.RouteTableIds = slices.Compact(tables)
		if m.policy != nil {
			rec.PolicyDocument = *m.policy
		}
		if m.dns != nil {
			rec.DnsRecordIpType = *m.dns
		}
		rec.Generation++
		if _, err := writeJSON(ctx, s.kv, recordKey(accountID, id), rec, rev); err != nil {
			if errors.Is(err, errConflict) {
				continue
			}
			return nil, err
		}
		return rec, s.releaseClaims(ctx, m.remove, id)
	}
	return nil, errors.Join(fmt.Errorf("modify %s: %w", id, errConflict), s.releaseClaims(ctx, taken, id))
}

// unlisted returns the tables of taken that rec does not list.
func unlisted(taken []string, rec *EndpointRecord) []string {
	return slices.DeleteFunc(slices.Clone(taken), rec.lists)
}

// remove fences an available record as deleting at the revision read, then
// removes it. It reports false when the endpoint was not available.
func (s *Store) remove(ctx context.Context, accountID, id string) (bool, error) {
	key := recordKey(accountID, id)
	for range moveAttempts {
		rec, rev, err := s.load(ctx, accountID, id)
		if err != nil || rec == nil || rec.State != StateAvailable {
			return false, err
		}
		rec.State = StateDeleting
		newRev, err := writeJSON(ctx, s.kv, key, rec, rev)
		if errors.Is(err, errConflict) {
			continue
		}
		if err != nil {
			return false, err
		}
		if err := deleteAt(ctx, s.kv, key, newRev); err != nil && !errors.Is(err, errConflict) {
			return true, err
		}
		return true, nil
	}
	return false, fmt.Errorf("delete %s: %w", id, errConflict)
}
