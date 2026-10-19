package spotinstance

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go/jetstream"
)

// LaunchReferences are the key-pair and placement-group names that an
// account's open or active spot requests would launch with. They stay inside
// EC2; callers must not report them.
type LaunchReferences struct {
	KeyNames            []string
	PlacementGroupNames []string
}

// AccountLaunchReferences reads every open or active spot request of the
// account. It is complete or an error: an unlistable bucket or an unreadable
// request is an error, because the unread request may name the resource.
func (s *SpotInstanceServiceImpl) AccountLaunchReferences(ctx context.Context, accountID string) (*LaunchReferences, error) {
	keys, err := kvutil.Keys(ctx, s.activeKV)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, fmt.Errorf("list spot requests: %w", err)
	}
	prefix := accountID + "."
	out := &LaunchReferences{}
	for _, key := range keys {
		if key == kvutil.VersionKey || !strings.HasPrefix(key, prefix) {
			continue
		}
		record, err := s.readRecord(ctx, s.activeKV, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue
		}
		if err != nil || record.Request == nil {
			return nil, errors.New("spot request unreadable")
		}
		switch aws.StringValue(record.Request.State) {
		case ec2.SpotInstanceStateClosed, ec2.SpotInstanceStateCancelled, ec2.SpotInstanceStateFailed:
			continue
		}
		spec := launchSpec(record.Request)
		if name := aws.StringValue(spec.KeyName); name != "" {
			out.KeyNames = append(out.KeyNames, name)
		}
		if spec.Placement != nil {
			if name := aws.StringValue(spec.Placement.GroupName); name != "" {
				out.PlacementGroupNames = append(out.PlacementGroupNames, name)
			}
		}
	}
	return out, nil
}
