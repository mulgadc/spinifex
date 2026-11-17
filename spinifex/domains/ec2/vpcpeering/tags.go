package vpcpeering

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

// ApplyRecordTags mirrors CreateTags into the caller's tag map on each named
// connection, so tag filters see tags added after create. Tags apply only in
// the account that wrote them.
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
		if res == nil || !strings.HasPrefix(*res, "pcx-") {
			continue
		}
		if err := s.updateTags(ctx, *res, accountID, mut); err != nil {
			slog.ErrorContext(ctx, "VPC peering tag mirror failed", "vpcPeeringConnectionId", *res, "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
	}
	return nil
}

// updateTags rewrites one account's tags on a live record at the revision read.
// An absent record, or one the account does not own, is skipped.
func (s *Store) updateTags(ctx context.Context, id, accountID string, mut func(map[string]string)) error {
	_, err := kvstore.Update(ctx, s.live, recordKey(id), kvstore.CASConfig{NotFound: errAbsent}, func(rec *PeeringRecord) (bool, error) {
		if !rec.involves(accountID) {
			return false, nil
		}
		tags := rec.Tags[accountID]
		if tags == nil {
			tags = map[string]string{}
		}
		mut(tags)
		if rec.Tags == nil {
			rec.Tags = map[string]map[string]string{}
		}
		if len(tags) == 0 {
			delete(rec.Tags, accountID)
		} else {
			rec.Tags[accountID] = tags
		}
		return true, nil
	})
	if errors.Is(err, errAbsent) {
		return nil
	}
	return err
}
