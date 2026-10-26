package elbv2

import (
	"context"
	"errors"
	"log/slog"
	"maps"

	"github.com/aws/aws-sdk-go/service/elbv2"
	resourcearn "github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
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
func (s *ELBv2ServiceImpl) resolveTaggable(ctx context.Context, arn string) (h taggable, notFoundError string, found bool, err error) {
	resourceType, terr := elbv2ResourceTypeFromArn(arn)
	if terr != nil {
		return taggable{}, "", false, terr
	}

	switch resourceType {
	case resourcearn.ELBv2LoadBalancer:
		notFoundError = awserrors.ErrorELBv2LoadBalancerNotFound
		lb, e := s.store.GetLoadBalancerByArn(ctx, arn)
		if e != nil {
			slog.Error("resolveTaggable: failed to get LB", "arn", arn, "err", e)
			return taggable{}, notFoundError, false, errors.New(awserrors.ErrorServerInternal)
		}
		if lb != nil {
			found = true
			h = taggable{tags: lb.Tags, owner: lb.AccountID, save: func(t map[string]string) error {
				lb.Tags = t
				return s.store.PutLoadBalancer(ctx, lb)
			}}
		}
	case resourcearn.ELBv2TargetGroup:
		notFoundError = awserrors.ErrorELBv2TargetGroupNotFound
		tg, e := s.store.GetTargetGroupByArn(ctx, arn)
		if e != nil {
			slog.Error("resolveTaggable: failed to get target group", "arn", arn, "err", e)
			return taggable{}, notFoundError, false, errors.New(awserrors.ErrorServerInternal)
		}
		if tg != nil {
			found = true
			// CAS the tag field alone: a whole-record Put here would discard
			// targets registered since this copy was read.
			h = taggable{tags: tg.Tags, owner: tg.AccountID, save: func(t map[string]string) error {
				_, uerr := s.store.UpdateTargetGroupByArn(ctx, arn, func(rec *TargetGroupRecord) (bool, error) {
					rec.Tags = t
					return true, nil
				})
				return uerr
			}}
		}
	case resourcearn.ELBv2Listener:
		notFoundError = awserrors.ErrorELBv2ListenerNotFound
		l, e := s.store.GetListenerByArn(ctx, arn)
		if e != nil {
			slog.Error("resolveTaggable: failed to get listener", "arn", arn, "err", e)
			return taggable{}, notFoundError, false, errors.New(awserrors.ErrorServerInternal)
		}
		if l != nil {
			found = true
			h = taggable{tags: l.Tags, owner: l.AccountID, save: func(t map[string]string) error {
				l.Tags = t
				return s.store.PutListener(ctx, l)
			}}
		}
	case resourcearn.ELBv2ListenerRule:
		notFoundError = awserrors.ErrorELBv2RuleNotFound
		r, e := s.store.GetRuleByArn(ctx, arn)
		if e != nil {
			slog.Error("resolveTaggable: failed to get rule", "arn", arn, "err", e)
			return taggable{}, notFoundError, false, errors.New(awserrors.ErrorServerInternal)
		}
		if r != nil {
			found = true
			h = taggable{tags: r.Tags, owner: r.AccountID, save: func(t map[string]string) error {
				r.Tags = t
				return s.store.PutRule(ctx, r)
			}}
		}
	}

	return h, notFoundError, found, nil
}

// resolvedTaggable pairs a resolved tag handle with the ARN it was loaded from.
type resolvedTaggable struct {
	arn string
	h   taggable
}

// resolveAllTaggable resolves every ARN and checks ownership before the caller
// writes anything, so an unknown or cross-account ARN anywhere in the request
// fails it with no resource modified. Repeated ARNs are resolved once.
func (s *ELBv2ServiceImpl) resolveAllTaggable(ctx context.Context, arns []*string, accountID string) ([]resolvedTaggable, error) {
	handles := make([]resolvedTaggable, 0, len(arns))
	seen := make(map[string]struct{}, len(arns))
	for _, arnPtr := range arns {
		if arnPtr == nil || *arnPtr == "" {
			return nil, errors.New(awserrors.ErrorMissingParameter)
		}
		arn := *arnPtr
		if _, dup := seen[arn]; dup {
			continue
		}
		seen[arn] = struct{}{}

		h, notFoundError, found, err := s.resolveTaggable(ctx, arn)
		if err != nil {
			return nil, err
		}
		if !found || h.owner != accountID {
			return nil, errors.New(notFoundError)
		}
		handles = append(handles, resolvedTaggable{arn: arn, h: h})
	}
	return handles, nil
}

// AddTags adds or overwrites tags on ELBv2 resources. Tags and every ARN are
// validated before any write so a bad key or ARN can't leave a partial apply;
// cross-account or unknown ARNs yield the per-resource not-found error.
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

	handles, err := s.resolveAllTaggable(ctx, input.ResourceArns, accountID)
	if err != nil {
		return nil, err
	}

	for _, rh := range handles {
		merged := rh.h.tags
		if merged == nil {
			merged = make(map[string]string, len(newTags))
		}
		maps.Copy(merged, newTags)
		if err := rh.h.save(merged); err != nil {
			slog.ErrorContext(ctx, "AddTags: failed to persist", "arn", rh.arn, "err", err)
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

	handles, err := s.resolveAllTaggable(ctx, input.ResourceArns, accountID)
	if err != nil {
		return nil, err
	}

	for _, rh := range handles {
		if len(rh.h.tags) == 0 {
			continue
		}
		for _, k := range keys {
			delete(rh.h.tags, k)
		}
		if err := rh.h.save(rh.h.tags); err != nil {
			slog.ErrorContext(ctx, "RemoveTags: failed to persist", "arn", rh.arn, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
	}

	return &elbv2.RemoveTagsOutput{}, nil
}
