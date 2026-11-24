package tgw

import (
	"context"
	"errors"
	"log/slog"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/tagmirror"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

var errAbsent = errors.New("tgw: record absent")

// ApplyRecordTags mirrors CreateTags into each named live record, so tag
// filters see tags added after create.
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
		if res == nil {
			continue
		}
		var err error
		switch id := *res; {
		case isAttachmentID(id):
			err = updateTags[AttachmentRecord](ctx, s.atts, recordKey(accountID, id), func(r *AttachmentRecord) *map[string]string { return &r.Tags }, mut)
		case isTransitGatewayID(id):
			err = updateTags[TransitGatewayRecord](ctx, s.tgws, recordKey(accountID, id), func(r *TransitGatewayRecord) *map[string]string { return &r.Tags }, mut)
		default:
			continue
		}
		if err != nil {
			slog.ErrorContext(ctx, "Transit Gateway tag mirror failed", "resourceId", *res, "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
	}
	return nil
}

// updateTags rewrites a live record's tags at the revision read; an absent
// record is skipped.
func updateTags[T any](ctx context.Context, kv jetstream.KeyValue, key string, tags func(*T) *map[string]string, mut func(map[string]string)) error {
	_, err := kvstore.Update(ctx, kv, key, kvstore.CASConfig{NotFound: errAbsent}, func(rec *T) (bool, error) {
		m := tags(rec)
		if *m == nil {
			*m = map[string]string{}
		}
		mut(*m)
		return true, nil
	})
	if errors.Is(err, errAbsent) {
		return nil
	}
	return err
}
