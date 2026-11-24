package tgw

import (
	"context"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/tagmirror"
)

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
			err = tagmirror.UpdateKVRecordTags(ctx, s.atts, accountID, id, func(r *AttachmentRecord) { mutTags(&r.Tags, mut) })
		case isTransitGatewayID(id):
			err = tagmirror.UpdateKVRecordTags(ctx, s.tgws, accountID, id, func(r *TransitGatewayRecord) { mutTags(&r.Tags, mut) })
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func mutTags(tags *map[string]string, mut func(map[string]string)) {
	if *tags == nil {
		*tags = map[string]string{}
	}
	mut(*tags)
}
