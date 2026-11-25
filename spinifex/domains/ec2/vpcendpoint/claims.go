package vpcendpoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
)

// claimLostError reports a table whose claim another endpoint holds and may
// keep, which AWS answers as the table already having the route.
type claimLostError struct{ table string }

func (e *claimLostError) Error() string { return "vpcendpoint: claim on " + e.table + " held" }

// errVpcGone reports a create whose VPC was deleted while it published.
var errVpcGone = errors.New("vpcendpoint: VPC deleted during create")

// takeClaim takes the S3 claim on table for rec, refreshing its own or taking
// over a stale one after fencing its holder, and returns the revision written.
func (s *Store) takeClaim(ctx context.Context, rec *EndpointRecord, table string) (uint64, error) {
	key := claimKey(table)
	mine := claim{VpceID: rec.ID, AccountID: rec.AccountID, ClaimedAt: s.now()}
	for range moveAttempts {
		rev, err := writeJSON(ctx, s.kv, key, mine, 0)
		if !errors.Is(err, errConflict) {
			return rev, err
		}
		held, heldRev, err := getJSON[claim](ctx, s.kv, key)
		if err != nil {
			return 0, err
		}
		if held == nil {
			continue
		}
		if held.VpceID != rec.ID {
			stale, retry, err := s.fenceHolder(ctx, held, table)
			if err != nil {
				return 0, err
			}
			if retry {
				continue
			}
			if !stale {
				return 0, &claimLostError{table: table}
			}
		}
		rev, err = writeJSON(ctx, s.kv, key, mine, heldRev)
		if !errors.Is(err, errConflict) {
			return rev, err
		}
	}
	return 0, fmt.Errorf("take %s: %w", key, errConflict)
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
// record is removed, and an available one is rewritten.
func (s *Store) fence(ctx context.Context, holder *EndpointRecord, rev uint64) error {
	key := recordKey(holder.AccountID, holder.ID)
	if holder.State == StatePending {
		return deleteAt(ctx, s.kv, key, rev)
	}
	_, err := writeJSON(ctx, s.kv, key, holder, rev)
	return err
}

// ownClaims returns the revision of each of tables' claims that names id.
func (s *Store) ownClaims(ctx context.Context, tables []string, id string) (map[string]uint64, error) {
	revs := map[string]uint64{}
	for _, t := range tables {
		c, rev, err := getJSON[claim](ctx, s.kv, claimKey(t))
		if err != nil {
			return nil, err
		}
		if c != nil && c.VpceID == id {
			revs[t] = rev
		}
	}
	return revs, nil
}

// releaseAt deletes each table's claim at the revision given. A newer
// revision is another request's refresh, and stays.
func (s *Store) releaseAt(ctx context.Context, revs map[string]uint64) error {
	var errs []error
	for t, rev := range revs {
		if err := deleteAt(ctx, s.kv, claimKey(t), rev); err != nil && !errors.Is(err, errConflict) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// create writes rec in the publication order: pending record, a re-read of
// the VPC, each claim, then available at the record's revision. A lost claim,
// or a fence by a taker, undoes the create and names the table it lost.
func (s *Store) create(ctx context.Context, rec *EndpointRecord) error {
	key := recordKey(rec.AccountID, rec.ID)
	rec.State = StatePending
	rev, err := writeJSON(ctx, s.kv, key, rec, 0)
	if err != nil {
		return err
	}
	taken := map[string]uint64{}
	vpc, err := s.vpcRecord(ctx, rec.AccountID, rec.VpcID)
	if err == nil && vpc == nil {
		err = errVpcGone
	}
	if err != nil {
		return errors.Join(err, s.abandon(ctx, rec, rev, taken))
	}
	for _, t := range rec.RouteTableIds {
		claimRev, err := s.takeClaim(ctx, rec, t)
		if err != nil {
			return errors.Join(err, s.abandon(ctx, rec, rev, taken))
		}
		taken[t] = claimRev
	}
	rec.State = StateAvailable
	_, err = writeJSON(ctx, s.kv, key, rec, rev)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errConflict):
		err = &claimLostError{table: s.lostTable(ctx, rec)}
	default:
		// The write may have landed: only this create makes its record available.
		cur, _, readErr := s.load(ctx, rec.AccountID, rec.ID)
		if readErr != nil {
			return errors.Join(err, readErr)
		}
		if cur != nil && cur.State == StateAvailable {
			return nil
		}
	}
	return errors.Join(err, s.abandon(ctx, rec, rev, taken))
}

// abandon removes a create that did not complete: its pending record if no
// taker removed it first, and the claims it took at the revisions written.
func (s *Store) abandon(ctx context.Context, rec *EndpointRecord, rev uint64, taken map[string]uint64) error {
	err := deleteAt(ctx, s.kv, recordKey(rec.AccountID, rec.ID), rev)
	if errors.Is(err, errConflict) {
		err = nil
	}
	return errors.Join(err, s.releaseAt(ctx, taken))
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
// revisions of removed tables' claims, the record at its revision, then the
// release of those claims at the revisions read. A failed modify releases
// nothing: another modify of the endpoint may publish a claim it refreshed.
func (s *Store) modify(ctx context.Context, accountID, id string, m modification) (*EndpointRecord, error) {
	for range moveAttempts {
		rec, rev, err := s.load(ctx, accountID, id)
		if err != nil {
			return nil, err
		}
		if rec == nil || rec.State != StateAvailable {
			return nil, errAbsent
		}
		if slices.ContainsFunc(m.remove, func(t string) bool { return !rec.lists(t) }) {
			return nil, errNotAssociated
		}
		for _, t := range m.add {
			if _, err := s.takeClaim(ctx, rec, t); err != nil {
				return nil, err
			}
		}
		released, err := s.ownClaims(ctx, m.remove, id)
		if err != nil {
			return nil, err
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
		if err := s.releaseAt(ctx, released); err != nil {
			slog.WarnContext(ctx, "VPC endpoint claim release failed; the sweep removes it", "vpcEndpointId", id, "err", err)
		}
		return rec, nil
	}
	return nil, fmt.Errorf("modify %s: %w", id, errConflict)
}

// remove fences an available record as deleting at the revision read, then
// finishes the delete. It reports false when the endpoint was not available.
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
		if err := s.finishDelete(ctx, rec, newRev); err != nil {
			slog.WarnContext(ctx, "VPC endpoint delete left for the sweep", "vpcEndpointId", id, "err", err)
		}
		return true, nil
	}
	return false, fmt.Errorf("delete %s: %w", id, errConflict)
}

// finishDelete drops a fenced endpoint's central tags, then its record. A
// failure leaves the deleting record, which the sweep finishes the same way.
func (s *Store) finishDelete(ctx context.Context, rec *EndpointRecord, rev uint64) error {
	if err := s.dropTags(ctx, rec.AccountID, rec.ID); err != nil {
		return err
	}
	err := deleteAt(ctx, s.kv, recordKey(rec.AccountID, rec.ID), rev)
	if errors.Is(err, errConflict) {
		return nil
	}
	return err
}

// releaseClaims deletes each of tables' claims while it names id, for a
// record already removed, whose claims no request can still depend on.
func (s *Store) releaseClaims(ctx context.Context, tables []string, id string) error {
	revs, err := s.ownClaims(ctx, tables, id)
	if err != nil {
		return err
	}
	return s.releaseAt(ctx, revs)
}
