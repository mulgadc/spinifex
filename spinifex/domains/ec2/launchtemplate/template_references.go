package launchtemplate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go/jetstream"
)

// TemplateReferences are the key-pair names, placement-group names and
// placement-group IDs that the account's live launch-template versions carry.
// They stay inside EC2; callers must not report them.
type TemplateReferences struct {
	KeyNames            []string
	PlacementGroupNames []string
	PlacementGroupIDs   []string
}

// AccountTemplateReferences reads every version body of every live template
// in the account, since RunInstances may resolve any of them by $Default,
// $Latest or number. It is complete or an error: an unlistable bucket, or a
// header or body it cannot read or decode, is an error, because the unread
// record may name the resource. Headerless bodies resolve from no launch.
func (s *LaunchTemplateServiceImpl) AccountTemplateReferences(ctx context.Context, accountID string) (*TemplateReferences, error) {
	keys, err := kvutil.Keys(ctx, s.kv)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, fmt.Errorf("list launch-template keys: %w", err)
	}
	prefix := accountID + "."
	var live []string
	for _, key := range keys {
		if !isHeaderKey(key, prefix) {
			continue
		}
		entry, err := s.kv.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return nil, errors.New("launch-template header unreadable")
		}
		var header LaunchTemplateHeader
		if json.Unmarshal(entry.Value(), &header) != nil || prefix+header.LaunchTemplateId != key {
			return nil, errors.New("launch-template header undecodable")
		}
		live = append(live, header.LaunchTemplateId)
	}

	out := &TemplateReferences{}
	for _, ltID := range live {
		bodyPrefix := versionPrefix(accountID, ltID)
		for _, key := range keys {
			if !strings.HasPrefix(key, bodyPrefix) {
				continue
			}
			if err := s.addVersionReferences(ctx, key, out); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// addVersionReferences adds what one version body would launch with. The
// body is never logged: it holds template data and user data.
func (s *LaunchTemplateServiceImpl) addVersionReferences(ctx context.Context, key string, out *TemplateReferences) error {
	entry, err := s.kv.Get(ctx, key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil
	}
	if err != nil {
		return errors.New("launch-template version unreadable")
	}
	var rec LaunchTemplateVersionRec
	if json.Unmarshal(entry.Value(), &rec) != nil || rec.Data == nil {
		return errors.New("launch-template version undecodable")
	}
	if name := aws.StringValue(rec.Data.KeyName); name != "" {
		out.KeyNames = append(out.KeyNames, name)
	}
	if p := rec.Data.Placement; p != nil {
		if name := aws.StringValue(p.GroupName); name != "" {
			out.PlacementGroupNames = append(out.PlacementGroupNames, name)
		}
		if id := aws.StringValue(p.GroupId); id != "" {
			out.PlacementGroupIDs = append(out.PlacementGroupIDs, id)
		}
	}
	return nil
}
