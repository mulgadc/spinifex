package handlers_iam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/mulgadc/spinifex/spinifex/arn"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// CanonicalResourceARN returns the ARN stored on a name-addressed IAM record.
// It avoids public IAM operations that perform membership or attachment scans.
func (s *IAMServiceImpl) CanonicalResourceARN(accountID string, kind arn.IAMResourceType, name string) (string, error) {
	ctx := context.Background()
	switch kind {
	case arn.IAMUser:
		resource, err := s.getUser(ctx, accountID, name)
		if err != nil {
			return "", err
		}
		return resource.ARN, nil
	case arn.IAMRole:
		resource, err := s.getRole(ctx, accountID, name)
		if err != nil {
			return "", err
		}
		return resource.ARN, nil
	case arn.IAMGroup:
		resource, err := s.getGroup(ctx, accountID, name)
		if err != nil {
			return "", err
		}
		return resource.ARN, nil
	case arn.IAMPolicy:
		resource, err := s.getPolicyByName(ctx, accountID, name)
		if err != nil {
			return "", err
		}
		return resource.ARN, nil
	case arn.IAMInstanceProfile:
		resource, err := s.getInstanceProfile(ctx, accountID, name)
		if err != nil {
			return "", err
		}
		return resource.ARN, nil
	default:
		return "", fmt.Errorf("unsupported IAM resource type %q", kind)
	}
}

func (s *IAMServiceImpl) getPolicyByName(ctx context.Context, accountID, policyName string) (*Policy, error) {
	entry, err := s.policiesBucket.Get(ctx, accountID+"."+policyName)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
		}
		return nil, fmt.Errorf("get policy: %w", err)
	}

	var policy Policy
	if err := json.Unmarshal(entry.Value(), &policy); err != nil {
		return nil, fmt.Errorf("unmarshal policy: %w", err)
	}
	return &policy, nil
}
