package vpcendpoint

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/tagmirror"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
)

// ApplyRecordTags mirrors CreateTags onto each named endpoint, so tag filters
// see tags added after create.
func (s *ServiceImpl) ApplyRecordTags(input *ec2.CreateTagsInput, accountID string) error {
	if input == nil {
		return nil
	}
	return s.mirrorTags(context.Background(), input.Resources, accountID, tagmirror.MergeTagsMut(input))
}

// RemoveRecordTags mirrors DeleteTags with AWS's delete semantics.
func (s *ServiceImpl) RemoveRecordTags(input *ec2.DeleteTagsInput, accountID string) error {
	if input == nil {
		return nil
	}
	return s.mirrorTags(context.Background(), input.Resources, accountID, tagmirror.RemoveTagsMut(input))
}

func (s *ServiceImpl) mirrorTags(ctx context.Context, resources []*string, accountID string, mut func(map[string]string)) error {
	for _, res := range resources {
		if res == nil || !strings.HasPrefix(*res, "vpce-") {
			continue
		}
		if err := s.updateTags(ctx, accountID, *res, mut); err != nil {
			slog.ErrorContext(ctx, "VPC endpoint tag mirror failed", "vpcEndpointId", *res, "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
	}
	return nil
}

// updateTags rewrites an available record's tags at the revision read. An
// absent record is skipped; tags change no association, so Generation stays.
func (s *Store) updateTags(ctx context.Context, accountID, id string, mut func(map[string]string)) error {
	_, err := kvstore.Update(ctx, s.kv, recordKey(accountID, id), kvstore.CASConfig{NotFound: errAbsent}, func(rec *EndpointRecord) (bool, error) {
		if rec.State != StateAvailable {
			return false, nil
		}
		if rec.Tags == nil {
			rec.Tags = map[string]string{}
		}
		mut(rec.Tags)
		return true, nil
	})
	if errors.Is(err, errAbsent) {
		return nil
	}
	return err
}

// CentralTagStore is the central tag index DescribeTags reads. Implemented by
// domains/ec2/tags.TagsServiceImpl.
type CentralTagStore interface {
	PutResourceTags(ctx context.Context, accountID, resourceID string, tags map[string]string) error
	DeleteAllTags(ctx context.Context, accountID, resourceID string) error
	ScanTaggedResources(ctx context.Context, prefix string) (map[string][]string, error)
}

// SetCentralTagStore injects the index that create writes and delete clears.
func (s *Store) SetCentralTagStore(st CentralTagStore) { s.centralTags = st }

// projectTags writes create-time tags to the central index. A failure is
// logged, not returned: the endpoint exists and its own describe has them.
func (s *Store) projectTags(ctx context.Context, accountID, id string, tags map[string]string) {
	if s.centralTags == nil || len(tags) == 0 {
		return
	}
	if err := s.centralTags.PutResourceTags(ctx, accountID, id, tags); err != nil {
		slog.ErrorContext(ctx, "VPC endpoint central tag write failed", "vpcEndpointId", id, "err", err)
	}
}

// dropTags clears a deleted endpoint's central tags; the sweep retries a failure.
func (s *Store) dropTags(ctx context.Context, accountID, id string) {
	if s.centralTags == nil {
		return
	}
	if err := s.centralTags.DeleteAllTags(ctx, accountID, id); err != nil {
		slog.WarnContext(ctx, "VPC endpoint central tag delete failed", "vpcEndpointId", id, "err", err)
	}
}

// sweepTags deletes central tags naming an endpoint with no record. Create
// writes the record before the tags, so a create in flight keeps its own.
func (s *Store) sweepTags(ctx context.Context, res *SweepResult) error {
	if s.centralTags == nil {
		return nil
	}
	byAccount, err := s.centralTags.ScanTaggedResources(ctx, "vpce-")
	if err != nil {
		return err
	}
	for account, ids := range byAccount {
		for _, id := range ids {
			rec, _, err := s.load(ctx, account, id)
			if err != nil {
				return err
			}
			if rec != nil {
				continue
			}
			if err := s.centralTags.DeleteAllTags(ctx, account, id); err != nil {
				return err
			}
			res.Keys++
		}
	}
	return nil
}
