package vpcpeering

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
// logged, not returned: the connection exists and its own describe has them.
func (s *Store) projectTags(ctx context.Context, accountID, id string, tags map[string]string) {
	if s.centralTags == nil || len(tags) == 0 {
		return
	}
	if err := s.centralTags.PutResourceTags(ctx, accountID, id, tags); err != nil {
		slog.ErrorContext(ctx, "VPC peering central tag write failed", "vpcPeeringConnectionId", id, "err", err)
	}
}

// sweepTags deletes central tag entries for connections in neither bucket, so
// a connection's tags go once its terminal record ages out. The live bucket is
// read first because a move writes the terminal record before the live delete.
func (s *Store) sweepTags(ctx context.Context, res *SweepResult) error {
	if s.centralTags == nil {
		return nil
	}
	byAccount, err := s.centralTags.ScanTaggedResources(ctx, "pcx-")
	if err != nil {
		return err
	}
	exists := map[string]bool{}
	for account, ids := range byAccount {
		for _, id := range ids {
			found, seen := exists[id]
			if !seen {
				if found, err = s.recordExists(ctx, id); err != nil {
					return err
				}
				exists[id] = found
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

func (s *Store) recordExists(ctx context.Context, id string) (bool, error) {
	rec, _, err := getJSON[PeeringRecord](ctx, s.live, recordKey(id))
	if err != nil || rec != nil {
		return rec != nil, err
	}
	rec, _, err = getJSON[PeeringRecord](ctx, s.term, recordKey(id))
	return rec != nil, err
}
