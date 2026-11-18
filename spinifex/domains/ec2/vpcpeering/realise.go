package vpcpeering

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// ListRecords reads every live connection record. It is for background
// passes only: the reconciler and the sweep.
func ListRecords(ctx context.Context, live jetstream.KeyValue) ([]PeeringRecord, error) {
	ids, err := listIndex(ctx, live, "pcx.")
	if err != nil {
		return nil, err
	}
	var out []PeeringRecord
	for _, id := range ids {
		rec, _, err := getJSON[PeeringRecord](ctx, live, recordKey(id))
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
	moved := false
	_, err := kvstore.Update(ctx, s.live, recordKey(id), kvstore.CASConfig{NotFound: errAbsent}, func(rec *PeeringRecord) (bool, error) {
		moved = rec.Status == StateProvisioning && rec.Generation == generation
		if moved {
			rec.Status, rec.StatusMessage = StateActive, activeMessage
			rec.RealizedGeneration = generation
		}
		return moved, nil
	})
	if errors.Is(err, errAbsent) {
		return nil
	}
	if err == nil && moved {
		slog.InfoContext(ctx, "VPC peering connection active", "vpcPeeringConnectionId", id, "generation", generation)
	}
	return err
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
	if err != nil || v == nil || !v.isOpen() {
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

// VpcBlocksDeletion reports whether vpcID has a provisioning or active
// connection, or is the accepter of a request pending from another account;
// either blocks DeleteVpc. It reads only the VPC's index.
func (s *Store) VpcBlocksDeletion(ctx context.Context, vpcID string) (bool, error) {
	conns, err := s.vpcConnections(ctx, vpcID)
	if err != nil {
		return false, err
	}
	for _, v := range conns {
		if v.isLinkedAlive() {
			return true, nil
		}
		if v.status == StatePendingAcceptance && v.rec.AccepterVpcID == vpcID && v.rec.RequesterAccountID != v.rec.AccepterAccountID {
			return true, nil
		}
	}
	return false, nil
}
