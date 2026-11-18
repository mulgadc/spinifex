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
