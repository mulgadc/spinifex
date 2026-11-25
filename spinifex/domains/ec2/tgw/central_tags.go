package tgw

import (
	"context"
	"log/slog"
)

// CentralTagStore is the central tag index DescribeTags reads. Implemented by
// domains/ec2/tags.TagsServiceImpl.
type CentralTagStore interface {
	PutResourceTags(ctx context.Context, accountID, resourceID string, tags map[string]string) error
	DeleteAllTags(ctx context.Context, accountID, resourceID string) error
	ScanTaggedResources(ctx context.Context, prefix string) (map[string][]string, error)
}

// SetCentralTagStore injects the index that create writes and the sweep clears.
func (s *Store) SetCentralTagStore(st CentralTagStore) { s.centralTags = st }

// projectTags writes create-time tags to the central index. A failure is
// logged, not returned: the resource exists and its own describe has them.
func (s *Store) projectTags(ctx context.Context, accountID, id string, tags map[string]string) {
	if s.centralTags == nil || len(tags) == 0 {
		return
	}
	if err := s.centralTags.PutResourceTags(ctx, accountID, id, tags); err != nil {
		slog.ErrorContext(ctx, "Transit Gateway central tag write failed", "resourceId", id, "err", err)
	}
}

// sweepTags deletes central tag entries for resources in neither bucket. The
// live bucket is read first because a move writes the terminal record before
// the live delete.
func (s *Store) sweepTags(ctx context.Context, res *SweepResult) error {
	if s.centralTags == nil {
		return nil
	}
	byAccount, err := s.centralTags.ScanTaggedResources(ctx, "tgw-")
	if err != nil {
		return err
	}
	for account, ids := range byAccount {
		for _, id := range ids {
			found, err := s.recordExists(ctx, account, id)
			if err != nil {
				return err
			}
			if found {
				continue
			}
			if err := s.centralTags.DeleteAllTags(ctx, account, id); err != nil {
				return err
			}
			res.TagsRemoved++
		}
	}
	return nil
}

// recordExists reports a record in either bucket. A tgw-rtb- ID names no
// record and is left alone.
func (s *Store) recordExists(ctx context.Context, accountID, id string) (bool, error) {
	var live, term bool
	var err error
	switch {
	case isAttachmentID(id):
		if live, err = exists[AttachmentRecord](ctx, s.atts, accountID, id); err == nil && !live {
			term, err = exists[AttachmentRecord](ctx, s.term, accountID, id)
		}
	case isTransitGatewayID(id):
		if live, err = exists[TransitGatewayRecord](ctx, s.tgws, accountID, id); err == nil && !live {
			term, err = exists[TransitGatewayRecord](ctx, s.term, accountID, id)
		}
	default:
		return true, nil
	}
	return live || term, err
}
