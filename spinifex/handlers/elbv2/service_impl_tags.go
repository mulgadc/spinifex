package handlers_elbv2

import (
	"context"
	"errors"
	"log/slog"
	"maps"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// taggable bundles a resource's current tags with a persist closure so the
// AddTags/RemoveTags mutation logic is identical across resource types.
type taggable struct {
	tags  map[string]string
	owner string
	save  func(map[string]string) error
}

// resolveTaggable loads the ELBv2 resource by ARN and returns a tag read/write handle.
// found is false when the resource doesn't exist; notFoundError is the per-type error.
// Store failures are logged and returned as ErrorServerInternal.
func (s *ELBv2ServiceImpl) resolveTaggable(arn string) (h taggable, notFoundError string, found bool, err error) {
	resourceType, terr := elbv2ResourceTypeFromArn(arn)
	if terr != nil {
		return taggable{}, "", false, terr
	}

	switch resourceType {
	case elbv2ResourceLoadBalancer:
		notFoundError = awserrors.ErrorELBv2LoadBalancerNotFound
		lb, e := s.store.GetLoadBalancerByArn(arn)
		if e != nil {
			slog.Error("resolveTaggable: failed to get LB", "arn", arn, "err", e)
			return taggable{}, notFoundError, false, errors.New(awserrors.ErrorServerInternal)
		}
		if lb != nil {
			found = true
			h = taggable{tags: lb.Tags, owner: lb.AccountID, save: func(t map[string]string) error {
				lb.Tags = t
				return s.store.PutLoadBalancer(lb)
			}}
		}
	case elbv2ResourceTargetGroup:
		notFoundError = awserrors.ErrorELBv2TargetGroupNotFound
		tg, e := s.store.GetTargetGroupByArn(arn)
		if e != nil {
			slog.Error("resolveTaggable: failed to get target group", "arn", arn, "err", e)
			return taggable{}, notFoundError, false, errors.New(awserrors.ErrorServerInternal)
		}
		if tg != nil {
			found = true
			h = taggable{tags: tg.Tags, owner: tg.AccountID, save: func(t map[string]string) error {
				tg.Tags = t
				return s.store.PutTargetGroup(tg)
			}}
		}
	case elbv2ResourceListener:
		notFoundError = awserrors.ErrorELBv2ListenerNotFound
		l, e := s.store.GetListenerByArn(arn)
		if e != nil {
			slog.Error("resolveTaggable: failed to get listener", "arn", arn, "err", e)
			return taggable{}, notFoundError, false, errors.New(awserrors.ErrorServerInternal)
		}
		if l != nil {
			found = true
			h = taggable{tags: l.Tags, owner: l.AccountID, save: func(t map[string]string) error {
				l.Tags = t
				return s.store.PutListener(l)
			}}
		}
	case elbv2ResourceListenerRule:
		notFoundError = awserrors.ErrorELBv2RuleNotFound
		r, e := s.store.GetRuleByArn(arn)
		if e != nil {
			slog.Error("resolveTaggable: failed to get rule", "arn", arn, "err", e)
			return taggable{}, notFoundError, false, errors.New(awserrors.ErrorServerInternal)
		}
		if r != nil {
			found = true
			h = taggable{tags: r.Tags, owner: r.AccountID, save: func(t map[string]string) error {
				r.Tags = t
				return s.store.PutRule(r)
			}}
		}
	}

	return h, notFoundError, found, nil
}

// AddTags adds or overwrites tags on ELBv2 resources. Tags are validated up-front
// so a malformed key can't leave a partial apply; cross-account or unknown ARNs
// yield the per-resource not-found error.
func (s *ELBv2ServiceImpl) AddTags(ctx context.Context, input *elbv2.AddTagsInput, accountID string) (*elbv2.AddTagsOutput, error) {
	if input == nil || len(input.ResourceArns) == 0 || len(input.Tags) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	newTags := make(map[string]string, len(input.Tags))
	for _, t := range input.Tags {
		if t == nil || t.Key == nil || *t.Key == "" {
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
		v := ""
		if t.Value != nil {
			v = *t.Value
		}
		newTags[*t.Key] = v
	}

	for _, arnPtr := range input.ResourceArns {
		if arnPtr == nil || *arnPtr == "" {
			return nil, errors.New(awserrors.ErrorMissingParameter)
		}
		arn := *arnPtr

		h, notFoundError, found, err := s.resolveTaggable(arn)
		if err != nil {
			return nil, err
		}
		if !found || h.owner != accountID {
			return nil, errors.New(notFoundError)
		}

		merged := h.tags
		if merged == nil {
			merged = make(map[string]string, len(newTags))
		}
		maps.Copy(merged, newTags)
		if err := h.save(merged); err != nil {
			slog.ErrorContext(ctx, "AddTags: failed to persist", "arn", arn, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
	}

	return &elbv2.AddTagsOutput{}, nil
}

// RemoveTags removes tag keys from ELBv2 resources; absent keys are silently ignored
// (idempotent, matching AWS). Cross-account or unknown ARNs yield the per-resource
// not-found error.
func (s *ELBv2ServiceImpl) RemoveTags(ctx context.Context, input *elbv2.RemoveTagsInput, accountID string) (*elbv2.RemoveTagsOutput, error) {
	if input == nil || len(input.ResourceArns) == 0 || len(input.TagKeys) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	keys := make([]string, 0, len(input.TagKeys))
	for _, k := range input.TagKeys {
		if k == nil || *k == "" {
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
		keys = append(keys, *k)
	}

	for _, arnPtr := range input.ResourceArns {
		if arnPtr == nil || *arnPtr == "" {
			return nil, errors.New(awserrors.ErrorMissingParameter)
		}
		arn := *arnPtr

		h, notFoundError, found, err := s.resolveTaggable(arn)
		if err != nil {
			return nil, err
		}
		if !found || h.owner != accountID {
			return nil, errors.New(notFoundError)
		}

		if len(h.tags) == 0 {
			continue
		}
		for _, k := range keys {
			delete(h.tags, k)
		}
		if err := h.save(h.tags); err != nil {
			slog.ErrorContext(ctx, "RemoveTags: failed to persist", "arn", arn, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
	}

	return &elbv2.RemoveTagsOutput{}, nil
}
