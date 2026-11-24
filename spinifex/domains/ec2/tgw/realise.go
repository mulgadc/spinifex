package tgw

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

var errAbsent = errors.New("tgw: record absent")

// listRecords reads every live record in kv. It is for background passes
// only: the reconciler, the sweep and the rollback drain.
func listRecords[T any](ctx context.Context, kv jetstream.KeyValue) ([]T, error) {
	keys, err := allKeys(ctx, kv)
	if err != nil {
		return nil, err
	}
	var out []T
	for _, key := range keys {
		if _, _, ok := recordID(key); !ok {
			continue
		}
		rec, _, err := getJSON[T](ctx, kv, key)
		if err != nil {
			return nil, err
		}
		if rec != nil {
			out = append(out, *rec)
		}
	}
	return out, nil
}

// ListRecords reads every record in the two live buckets, for network's
// projection. It never creates a bucket.
func ListRecords(ctx context.Context, tgws, atts jetstream.KeyValue) ([]TransitGatewayRecord, []AttachmentRecord, error) {
	gws, err := listRecords[TransitGatewayRecord](ctx, tgws)
	if err != nil {
		return nil, nil, fmt.Errorf("list %s: %w", KVBucketTransitGateways, err)
	}
	as, err := listRecords[AttachmentRecord](ctx, atts)
	if err != nil {
		return nil, nil, fmt.Errorf("list %s: %w", KVBucketAttachments, err)
	}
	return gws, as, nil
}

// MarkTransitGatewayAvailable confirms the router is realised at generation.
// It moves only pending at that generation, so a late confirmation after a
// delete changes nothing.
func (s *Store) MarkTransitGatewayAvailable(ctx context.Context, accountID, id string, generation int64) error {
	_, err := kvstore.Update(ctx, s.tgws, recordKey(accountID, id), kvstore.CASConfig{NotFound: errAbsent}, func(rec *TransitGatewayRecord) (bool, error) {
		if rec.State != StatePending || rec.Generation != generation {
			return false, nil
		}
		rec.State, rec.RealizedGeneration = StateAvailable, generation
		return true, nil
	})
	if errors.Is(err, errAbsent) {
		return nil
	}
	return err
}

// MarkTransitGatewayCleaned records that the router is gone: a deleting
// Transit Gateway moves to the terminal bucket as deleted.
func (s *Store) MarkTransitGatewayCleaned(ctx context.Context, accountID, id string) error {
	for range moveAttempts {
		rec, rev, err := s.transitGateway(ctx, accountID, id)
		if err != nil || rec == nil {
			return err
		}
		switch rec.State {
		case StateDeleting:
			rec.State, rec.TerminalAt, rec.RealizedGeneration = StateDeleted, s.now(), rec.Generation
			err = s.moveTransitGatewayTerminal(ctx, rec, rev)
		case StateDeleted:
			err = s.finishTransitGatewayMove(ctx, rec, rev)
		default:
			return nil
		}
		if errors.Is(err, errConflict) {
			continue
		}
		if err == nil {
			slog.InfoContext(ctx, "Transit Gateway deleted", "transitGatewayId", id)
		}
		return err
	}
	return fmt.Errorf("mark %s cleaned: %w", id, errConflict)
}

// PrepareAttachment readies a pending attachment for realization: it rewrites
// each parent at the revision read, reaffirms its keys, then takes a link slot.
// An absent or marked parent, a lost key, or a full pool moves it to failed. It returns
// the record to realise, or nil when there is none.
func (s *Store) PrepareAttachment(ctx context.Context, accountID, id string) (*AttachmentRecord, error) {
	for range moveAttempts {
		rec, rev, err := s.attachment(ctx, accountID, id)
		if err != nil || rec == nil || rec.State != StatePending {
			return nil, err
		}
		present, err := s.touchParents(ctx, rec)
		if errors.Is(err, errConflict) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !present {
			return nil, s.failAttachment(ctx, rec, rev, "a parent is gone")
		}
		// An admission that stopped after its record write never confirmed its keys.
		if err := s.reaffirmAttachmentKeys(ctx, rec); err != nil {
			if errors.Is(err, errKeyLost) {
				return nil, s.failAttachment(ctx, rec, rev, "an index key or claim is lost")
			}
			return nil, err
		}
		if rec.LinkPrefix != "" {
			return rec, nil
		}
		prefix, key, claimRev, err := s.takeLinkSlot(ctx, rec)
		if errors.Is(err, errLinkSpaceExhausted) {
			return nil, s.failAttachment(ctx, rec, rev, "no free link address")
		}
		if err != nil {
			return nil, err
		}
		rec.LinkPrefix = prefix
		newRev, err := writeJSON(ctx, s.atts, recordKey(accountID, id), rec, rev)
		if errors.Is(err, errConflict) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := touchAt(ctx, s.atts, key, claimRev); err != nil {
			if !errors.Is(err, errConflict) {
				return nil, err
			}
			// The slot was taken over: drop the prefix and take another.
			rec.LinkPrefix = ""
			if _, err := writeJSON(ctx, s.atts, recordKey(accountID, id), rec, newRev); err != nil && !errors.Is(err, errConflict) {
				return nil, err
			}
			continue
		}
		return rec, nil
	}
	return nil, fmt.Errorf("prepare %s: %w", id, errConflict)
}

// touchParents rewrites the Transit Gateway, VPC and subnets at the revisions
// read. present is false when one is absent, marked or not available.
func (s *Store) touchParents(ctx context.Context, rec *AttachmentRecord) (present bool, err error) {
	acct := rec.AccountID
	tgw, rev, err := s.transitGateway(ctx, acct, rec.TransitGatewayID)
	if err != nil || tgw == nil || tgw.State != StateAvailable {
		return false, err
	}
	if err := touchAt(ctx, s.tgws, recordKey(acct, tgw.ID), rev); err != nil {
		return false, err
	}
	vpc, rev, err := s.parents.VPC(ctx, acct, rec.VpcID)
	if err != nil || vpc == nil || vpc.Deleting {
		return false, err
	}
	if err := parentTouch(s.parents.TouchVPC(ctx, acct, rec.VpcID, rev)); err != nil {
		return false, err
	}
	for _, id := range rec.SubnetIDs {
		sub, rev, err := s.parents.Subnet(ctx, acct, id)
		if err != nil || sub == nil || sub.Deleting || sub.VpcId != rec.VpcID {
			return false, err
		}
		if err := parentTouch(s.parents.TouchSubnet(ctx, acct, id, rev)); err != nil {
			return false, err
		}
	}
	return true, nil
}

// parentTouch maps the vpc domain's lost race to this package's.
func parentTouch(err error) error {
	if errors.Is(err, ec2vpc.ErrParentChanged) {
		return errConflict
	}
	return err
}

func (s *Store) failAttachment(ctx context.Context, rec *AttachmentRecord, rev uint64, why string) error {
	rec.State, rec.TerminalAt = StateFailed, s.now()
	rec.Generation++
	err := s.moveAttachmentTerminal(ctx, rec, rev)
	if errors.Is(err, errConflict) {
		return nil
	}
	if err == nil {
		slog.WarnContext(ctx, "Transit Gateway attachment failed", "transitGatewayAttachmentId", rec.ID, "reason", why)
	}
	return err
}

// MarkAttachmentAvailable confirms the link is realised at generation.
func (s *Store) MarkAttachmentAvailable(ctx context.Context, accountID, id string, generation int64) error {
	_, err := kvstore.Update(ctx, s.atts, recordKey(accountID, id), kvstore.CASConfig{NotFound: errAbsent}, func(rec *AttachmentRecord) (bool, error) {
		if rec.State != StatePending || rec.Generation != generation || rec.LinkPrefix == "" {
			return false, nil
		}
		rec.State, rec.RealizedGeneration = StateAvailable, generation
		return true, nil
	})
	if errors.Is(err, errAbsent) {
		return nil
	}
	return err
}

// MarkAttachmentCleaned records that the link is gone: a deleting attachment
// moves to the terminal bucket as deleted and releases its claims and keys.
func (s *Store) MarkAttachmentCleaned(ctx context.Context, accountID, id string) error {
	for range moveAttempts {
		rec, rev, err := s.attachment(ctx, accountID, id)
		if err != nil || rec == nil {
			return err
		}
		switch rec.State {
		case StateDeleting:
			rec.State, rec.TerminalAt, rec.RealizedGeneration = StateDeleted, s.now(), rec.Generation
			err = s.moveAttachmentTerminal(ctx, rec, rev)
		case StateDeleted, StateFailed:
			err = s.finishAttachmentMove(ctx, rec, rev)
		default:
			return nil
		}
		if errors.Is(err, errConflict) {
			continue
		}
		if err == nil {
			slog.InfoContext(ctx, "Transit Gateway attachment deleted", "transitGatewayAttachmentId", id)
		}
		return err
	}
	return fmt.Errorf("mark %s cleaned: %w", id, errConflict)
}

// pairAttachment reads the attachment holding the VPC's pair claim.
func (s *Store) pairAttachment(ctx context.Context, tgwID, vpcID string) (*AttachmentRecord, error) {
	held, _, err := getJSON[claim](ctx, s.atts, pairKey(tgwID, vpcID))
	if err != nil || held == nil {
		return nil, err
	}
	rec, _, err := s.attachment(ctx, held.AccountID, held.Owner)
	return rec, err
}

// RouteTarget reports whether a route in vpcID's table may target tgwID: the
// Transit Gateway is available and the VPC has a pending or available
// attachment to it. It reads two records.
func (s *Store) RouteTarget(ctx context.Context, accountID, tgwID, vpcID string) (bool, error) {
	if !isTransitGatewayID(tgwID) {
		return false, nil
	}
	tgw, _, err := s.transitGateway(ctx, accountID, tgwID)
	if err != nil || tgw == nil || tgw.State != StateAvailable {
		return false, err
	}
	att, err := s.pairAttachment(ctx, tgwID, vpcID)
	if err != nil || att == nil || att.AccountID != accountID {
		return false, err
	}
	return att.State == StatePending || att.State == StateAvailable, nil
}

// RouteActive reports whether a route from vpcID to tgwID forwards: both the
// Transit Gateway and the attachment are available. Otherwise it is blackhole.
func (s *Store) RouteActive(ctx context.Context, accountID, tgwID, vpcID string) (bool, error) {
	tgw, _, err := s.transitGateway(ctx, accountID, tgwID)
	if err != nil || tgw == nil || tgw.State != StateAvailable {
		return false, err
	}
	att, err := s.pairAttachment(ctx, tgwID, vpcID)
	if err != nil || att == nil {
		return false, err
	}
	return att.AccountID == accountID && att.State == StateAvailable, nil
}

// VpcBlocksDeletion reports a live attachment of the VPC, or a young index
// key whose record is not yet written. It reads only the VPC's index.
func (s *Store) VpcBlocksDeletion(ctx context.Context, _, vpcID string) (bool, error) {
	ids, err := s.liveAttachmentsUnder(ctx, "vpc."+vpcID+".", 1)
	return len(ids) > 0, err
}

// SubnetBlocksDeletion is VpcBlocksDeletion for a subnet.
func (s *Store) SubnetBlocksDeletion(ctx context.Context, _, subnetID string) (bool, error) {
	ids, err := s.liveAttachmentsUnder(ctx, "subnet."+subnetID+".", 1)
	return len(ids) > 0, err
}

// CountLive counts Transit Gateway records not yet deleted, for the rollback
// drain. It never creates the bucket.
func CountLive(ctx context.Context, js jetstream.JetStream) (int, error) {
	kv, err := js.KeyValue(ctx, KVBucketTransitGateways)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", KVBucketTransitGateways, err)
	}
	recs, err := listRecords[TransitGatewayRecord](ctx, kv)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, rec := range recs {
		if rec.State != StateDeleted {
			n++
		}
	}
	return n, nil
}

// AccountResources lists an account's attachments and Transit Gateways that
// are not deleted, for account deletion. It reads the account's index only.
func (s *Store) AccountResources(ctx context.Context, accountID string) (attachments []AttachmentRecord, tgws []TransitGatewayRecord, err error) {
	prefix := "acct." + accountID + "."
	attIDs, err := listIndex(ctx, s.atts, prefix)
	if err != nil {
		return nil, nil, err
	}
	for _, id := range attIDs {
		rec, _, err := s.attachment(ctx, accountID, id)
		if err != nil {
			return nil, nil, err
		}
		if rec != nil {
			attachments = append(attachments, *rec)
		}
	}
	tgwIDs, err := listIndex(ctx, s.tgws, prefix)
	if err != nil {
		return nil, nil, err
	}
	for _, id := range tgwIDs {
		rec, _, err := s.transitGateway(ctx, accountID, id)
		if err != nil {
			return nil, nil, err
		}
		if rec != nil {
			tgws = append(tgws, *rec)
		}
	}
	return attachments, tgws, nil
}
