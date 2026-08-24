package handlers_iam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/mulgadc/spinifex/spinifex/arn"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// canonicalARNAttempts bounds retries of a transient JetStream fault. This read
// gates every name-addressed IAM request, so a leader election that the policy
// snapshot resolver rides out must not fail the request here instead.
const canonicalARNAttempts = 3

// CanonicalResourceARN returns the ARN stored on a name-addressed IAM record.
// It avoids public IAM operations that perform membership or attachment scans.
func (s *IAMServiceImpl) CanonicalResourceARN(accountID string, kind arn.IAMResourceType, name string) (string, error) {
	var resource string
	var err error
	for attempt := range canonicalARNAttempts {
		resource, err = s.canonicalResourceARN(accountID, kind, name)
		if err == nil || !isNATSTransient(err) {
			break
		}
		if attempt < canonicalARNAttempts-1 {
			slog.Debug("CanonicalResourceARN: transient NATS error, retrying",
				"accountID", accountID, "kind", kind, "name", name, "attempt", attempt+1, "err", err)
			time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
		}
	}
	return resource, err
}

func isNATSTransient(err error) bool {
	return err != nil && (errors.Is(err, nats.ErrNoResponders) ||
		errors.Is(err, nats.ErrTimeout) ||
		errors.Is(err, nats.ErrNoStreamResponse))
}

func (s *IAMServiceImpl) canonicalResourceARN(accountID string, kind arn.IAMResourceType, name string) (string, error) {
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
