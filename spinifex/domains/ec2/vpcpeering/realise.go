package vpcpeering

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nats-io/nats.go/jetstream"
)

// ListRecords reads every live connection record. It is for background
// passes only: the reconciler and the sweep.
func ListRecords(ctx context.Context, live jetstream.KeyValue) ([]PeeringRecord, error) {
	lister, err := live.ListKeysFiltered(ctx, "pcx.*")
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("list peering records: %w", err)
	}
	var keys []string
	for k := range lister.Keys() {
		keys = append(keys, k)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []PeeringRecord
	for _, k := range keys {
		rec, _, err := getJSON[PeeringRecord](ctx, live, k)
		if err != nil {
			return nil, err
		}
		if rec != nil {
			out = append(out, *rec)
		}
	}
	return out, nil
}

// MarkActive confirms the reconciler realised the link at generation. It only
// moves provisioning to active at that generation, so a confirmation landing
// after a delete or a newer accept changes nothing.
func (s *Store) MarkActive(ctx context.Context, id string, generation int64) error {
	for range moveAttempts {
		rec, rev, err := getJSON[PeeringRecord](ctx, s.live, recordKey(id))
		if err != nil {
			return err
		}
		if rec == nil || rec.Status != StateProvisioning || rec.Generation != generation {
			return nil
		}
		rec.Status, rec.StatusMessage = StateActive, activeMessage
		rec.RealizedGeneration = generation
		if _, err := updateJSON(ctx, s.live, recordKey(id), rec, rev); err != nil {
			if errors.Is(err, errConflict) {
				continue
			}
			return err
		}
		slog.InfoContext(ctx, "VPC peering connection active", "vpcPeeringConnectionId", id, "generation", generation)
		return nil
	}
	return fmt.Errorf("mark %s active: %w", id, errConflict)
}

// MarkCleaned records that the reconciler removed a connection's link and
// guards: a deleted record, or a linked one whose VPC is gone, moves to the
// terminal bucket and releases its claims and keys.
func (s *Store) MarkCleaned(ctx context.Context, id string) error {
	for range moveAttempts {
		rec, rev, err := getJSON[PeeringRecord](ctx, s.live, recordKey(id))
		if err != nil {
			return err
		}
		if rec == nil {
			return nil
		}
		switch {
		case rec.Status == StateDeleted && rec.CleanupPending:
		case rec.Status == StateProvisioning || rec.Status == StateActive:
			owner, err := s.missingVpcOwner(ctx, rec)
			if err != nil {
				return err
			}
			if owner == "" {
				return nil
			}
			rec.Status, rec.StatusMessage = StateDeleted, deletedMessage(owner)
			rec.TerminalAt, rec.TerminatedBy = s.now(), owner
		default:
			return nil
		}
		rec.CleanupPending = false
		rec.RealizedGeneration = rec.Generation
		if err := s.moveTerminal(ctx, rec, rev); err != nil {
			if errors.Is(err, errConflict) {
				continue
			}
			return err
		}
		slog.InfoContext(ctx, "VPC peering connection cleaned up", "vpcPeeringConnectionId", id)
		return nil
	}
	return fmt.Errorf("mark %s cleaned: %w", id, errConflict)
}

// RouteTarget is a connection as CreateRoute and ReplaceRoute see it.
type RouteTarget struct {
	RequesterVpcID string
	AccepterVpcID  string
}

// RouteTarget returns the connection a route may target: visible to the
// caller and pending, provisioning or active. Nil otherwise.
func (s *Store) RouteTarget(ctx context.Context, accountID, id string) (*RouteTarget, error) {
	if !strings.HasPrefix(id, "pcx-") {
		return nil, nil
	}
	v, err := s.visible(ctx, id, accountID)
	if err != nil || v == nil || !v.live || !v.isOpen() {
		return nil, err
	}
	return &RouteTarget{RequesterVpcID: v.rec.RequesterVpcID, AccepterVpcID: v.rec.AccepterVpcID}, nil
}

// ActiveConnections reports which of ids are active, for a route's derived
// state; any other connection's routes describe blackhole.
func (s *Store) ActiveConnections(ctx context.Context, ids []string) (map[string]bool, error) {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		if _, done := out[id]; done {
			continue
		}
		rec, _, err := getJSON[PeeringRecord](ctx, s.live, recordKey(id))
		if err != nil {
			return nil, err
		}
		out[id] = rec != nil && rec.Status == StateActive
	}
	return out, nil
}

// VpcHasLinkedConnection reports whether vpcID has a provisioning or active
// connection, which blocks DeleteVpc. It reads only the VPC's index.
func (s *Store) VpcHasLinkedConnection(ctx context.Context, vpcID string) (bool, error) {
	conns, err := s.vpcConnections(ctx, vpcID)
	if err != nil {
		return false, err
	}
	for _, v := range conns {
		if v.isLinkedAlive() {
			return true, nil
		}
	}
	return false, nil
}

// ReapAccount ends every live connection accountID requested or accepted, for
// account termination: pending requests move to the terminal bucket and linked
// connections record the cleanup obligation the reconciler finishes.
func (s *Store) ReapAccount(ctx context.Context, accountID string) error {
	ids, err := listIndex(ctx, s.live, "acct."+accountID+".")
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.reapOne(ctx, accountID, id); err != nil {
			return fmt.Errorf("reap %s: %w", id, err)
		}
	}
	return nil
}

func (s *Store) reapOne(ctx context.Context, accountID, id string) error {
	for range moveAttempts {
		v, err := s.load(ctx, id)
		if err != nil {
			return err
		}
		if v == nil || !v.live {
			return nil
		}
		rec := v.rec
		rec.Generation++
		rec.TerminalAt, rec.TerminatedBy = s.now(), accountID
		switch {
		case v.status == StatePendingAcceptance && accountID == rec.RequesterAccountID:
			rec.Status, rec.StatusMessage = StateDeleted, deletedMessage(accountID)
			err = s.moveTerminal(ctx, &rec, v.rev)
		case v.status == StatePendingAcceptance:
			rec.Status, rec.StatusMessage = StateRejected, rejectedMessage(accountID)
			err = s.moveTerminal(ctx, &rec, v.rev)
		case v.isLinkedAlive():
			rec.Status, rec.StatusMessage = StateDeleted, deletedMessage(accountID)
			rec.CleanupPending = true
			if _, err = updateJSON(ctx, s.live, recordKey(id), &rec, v.rev); err == nil {
				err = s.releaseClaim(ctx, pairKey(rec.RequesterVpcID, rec.AccepterVpcID), id)
			}
		default:
			// Already deleted awaiting cleanup, or terminal awaiting the sweep.
			return nil
		}
		if errors.Is(err, errConflict) {
			continue
		}
		return err
	}
	return errConflict
}
