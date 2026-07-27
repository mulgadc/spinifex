package handlers_elbv2

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// attrPair is the internal key/value shape the generic attribute handlers work
// in, decoupled from the two near-identical SDK attribute types
// (*elbv2.TargetGroupAttribute and *elbv2.LoadBalancerAttribute).
type attrPair struct {
	Key   string
	Value string
}

// attrResource bundles the per-resource-type plumbing the generic Modify/
// Describe attribute handlers need, so the target-group and load-balancer
// paths share one implementation.
type attrResource[R any] struct {
	arn         string
	accountID   string
	opName      string // slog prefix / error context
	notFound    string // awserrors code returned for missing/cross-account
	fetch       func(string) (*R, error)
	save        func(*R) error
	accountIDOf func(*R) string
	defaults    func(*R) map[string]string
	attrsOf     func(*R) map[string]string
	setAttrs    func(*R, map[string]string)
}

// load fetches and authorises the record. An empty accountID is rejected
// explicitly so a direct NATS/internal caller cannot match a record whose
// AccountID is also empty.
func (r attrResource[R]) load() (*R, error) {
	rec, err := r.fetch(r.arn)
	if err != nil {
		slog.Error(r.opName+": failed to load record", "arn", r.arn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if rec == nil || r.accountID == "" || r.accountIDOf(rec) != r.accountID {
		return nil, errors.New(r.notFound)
	}
	return rec, nil
}

// modifyResourceAttributes validates and merges submitted attributes, persisting only
// on change. rawCount is pre-nil-filter count, used to distinguish "nothing sent"
// from "everything sent was invalid".
func modifyResourceAttributes[R any](res attrResource[R], pairs []attrPair, rawCount int) ([]attrPair, error) {
	if res.arn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	rec, err := res.load()
	if err != nil {
		return nil, err
	}

	known := res.defaults(rec)
	attrs := res.attrsOf(rec)
	submitted := make([]attrPair, 0, len(pairs))
	dirty := false
	for _, p := range pairs {
		if _, ok := known[p.Key]; !ok {
			slog.Warn(res.opName+": rejecting unknown attribute key", "arn", res.arn, "key", p.Key)
			return nil, errors.New(awserrors.ErrorValidationError)
		}
		submitted = append(submitted, p)
		if existing, ok := attrs[p.Key]; ok && existing == p.Value {
			continue // value already matches stored — no mutation needed
		}
		if attrs == nil {
			attrs = make(map[string]string)
		}
		attrs[p.Key] = p.Value
		dirty = true
	}

	// Caller sent attributes but every one was dropped by nil filtering: surface
	// an error rather than a successful empty response the caller misreads as a
	// landed write.
	if rawCount > 0 && len(submitted) == 0 {
		slog.Warn(res.opName+": all submitted attributes were invalid", "arn", res.arn, "submitted_count", rawCount)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	// Skip the NATS/KV write when nothing changed. Terraform re-applies the same
	// attribute set on every drift check, so this kills ~all Modify traffic
	// during steady state and narrows the read-modify-write race window.
	if dirty {
		res.setAttrs(rec, attrs)
		if err := res.save(rec); err != nil {
			slog.Error(res.opName+": failed to persist", "arn", res.arn, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
	}

	return submitted, nil
}

// describeResourceAttributes returns the record's attributes merged over the
// per-type defaults, sorted by key for deterministic output (Terraform diffs
// and snapshot tests depend on stable ordering).
func describeResourceAttributes[R any](res attrResource[R]) ([]attrPair, error) {
	if res.arn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	rec, err := res.load()
	if err != nil {
		return nil, err
	}

	merged := res.defaults(rec)
	maps.Copy(merged, res.attrsOf(rec))

	keys := slices.Sorted(maps.Keys(merged))
	out := make([]attrPair, 0, len(keys))
	for _, k := range keys {
		out = append(out, attrPair{Key: k, Value: merged[k]})
	}
	return out, nil
}

// sdkAttrsIn converts an SDK attribute slice into internal pairs, dropping nil
// elements and elements with a nil Key/Value. Returns the cleaned pairs and the
// raw submitted count.
func sdkAttrsIn[A any](in []A, keyVal func(A) (*string, *string), arn, opName string) ([]attrPair, int) {
	out := make([]attrPair, 0, len(in))
	for _, a := range in {
		k, v := keyVal(a)
		if k == nil || v == nil {
			slog.Warn(opName+": skipping invalid attribute element", "arn", arn)
			continue
		}
		out = append(out, attrPair{Key: *k, Value: *v})
	}
	return out, len(in)
}

// sdkAttrsOut converts internal pairs back into a fresh SDK attribute slice via
// mk. Fresh strings are allocated (never aliasing caller input). Returns nil for
// an empty set to match AWS for resources with no submitted attributes.
func sdkAttrsOut[A any](pairs []attrPair, mk func(key, value string) A) []A {
	if len(pairs) == 0 {
		return nil
	}
	out := make([]A, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, mk(p.Key, p.Value))
	}
	return out
}

func (s *ELBv2ServiceImpl) tgAttrResource(ctx context.Context, arn, accountID, opName string) attrResource[TargetGroupRecord] {
	return attrResource[TargetGroupRecord]{
		arn:       arn,
		accountID: accountID,
		opName:    opName,
		notFound:  awserrors.ErrorELBv2TargetGroupNotFound,
		// fetch/save close over ctx: attrResource is consumed by the generic
		// attribute helpers, which have no context of their own to pass down.
		fetch:       func(arn string) (*TargetGroupRecord, error) { return s.store.GetTargetGroupByArn(ctx, arn) },
		save:        func(tg *TargetGroupRecord) error { return s.store.PutTargetGroup(ctx, tg) },
		accountIDOf: func(r *TargetGroupRecord) string { return r.AccountID },
		defaults:    func(*TargetGroupRecord) map[string]string { return DefaultTargetGroupAttributes() },
		attrsOf:     func(r *TargetGroupRecord) map[string]string { return r.Attributes },
		setAttrs:    func(r *TargetGroupRecord, m map[string]string) { r.Attributes = m },
	}
}

func (s *ELBv2ServiceImpl) lbAttrResource(ctx context.Context, arn, accountID, opName string) attrResource[LoadBalancerRecord] {
	return attrResource[LoadBalancerRecord]{
		arn:       arn,
		accountID: accountID,
		opName:    opName,
		notFound:  awserrors.ErrorELBv2LoadBalancerNotFound,
		// See tgAttrResource: the generic helpers carry no context.
		fetch:       func(arn string) (*LoadBalancerRecord, error) { return s.store.GetLoadBalancerByArn(ctx, arn) },
		save:        func(lb *LoadBalancerRecord) error { return s.store.PutLoadBalancer(ctx, lb) },
		accountIDOf: func(r *LoadBalancerRecord) string { return r.AccountID },
		defaults:    func(r *LoadBalancerRecord) map[string]string { return DefaultLoadBalancerAttributes(r.Type) },
		attrsOf:     func(r *LoadBalancerRecord) map[string]string { return r.Attributes },
		setAttrs:    func(r *LoadBalancerRecord, m map[string]string) { r.Attributes = m },
	}
}

func tgAttrKeyVal(a *elbv2.TargetGroupAttribute) (*string, *string) {
	if a == nil {
		return nil, nil
	}
	return a.Key, a.Value
}

func lbAttrKeyVal(a *elbv2.LoadBalancerAttribute) (*string, *string) {
	if a == nil {
		return nil, nil
	}
	return a.Key, a.Value
}

func mkTGAttr(key, value string) *elbv2.TargetGroupAttribute {
	return &elbv2.TargetGroupAttribute{Key: aws.String(key), Value: aws.String(value)}
}

func mkLBAttr(key, value string) *elbv2.LoadBalancerAttribute {
	return &elbv2.LoadBalancerAttribute{Key: aws.String(key), Value: aws.String(value)}
}

func (s *ELBv2ServiceImpl) ModifyTargetGroupAttributes(ctx context.Context, input *elbv2.ModifyTargetGroupAttributesInput, accountID string) (*elbv2.ModifyTargetGroupAttributesOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	const op = "ModifyTargetGroupAttributes"
	arn := aws.StringValue(input.TargetGroupArn)
	pairs, raw := sdkAttrsIn(input.Attributes, tgAttrKeyVal, arn, op)
	submitted, err := modifyResourceAttributes(s.tgAttrResource(ctx, arn, accountID, op), pairs, raw)
	if err != nil {
		return nil, err
	}
	return &elbv2.ModifyTargetGroupAttributesOutput{Attributes: sdkAttrsOut(submitted, mkTGAttr)}, nil
}

func (s *ELBv2ServiceImpl) DescribeTargetGroupAttributes(ctx context.Context, input *elbv2.DescribeTargetGroupAttributesInput, accountID string) (*elbv2.DescribeTargetGroupAttributesOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	const op = "DescribeTargetGroupAttributes"
	arn := aws.StringValue(input.TargetGroupArn)
	pairs, err := describeResourceAttributes(s.tgAttrResource(ctx, arn, accountID, op))
	if err != nil {
		return nil, err
	}
	return &elbv2.DescribeTargetGroupAttributesOutput{Attributes: sdkAttrsOut(pairs, mkTGAttr)}, nil
}

func (s *ELBv2ServiceImpl) ModifyLoadBalancerAttributes(ctx context.Context, input *elbv2.ModifyLoadBalancerAttributesInput, accountID string) (*elbv2.ModifyLoadBalancerAttributesOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	const op = "ModifyLoadBalancerAttributes"
	arn := aws.StringValue(input.LoadBalancerArn)
	pairs, raw := sdkAttrsIn(input.Attributes, lbAttrKeyVal, arn, op)
	submitted, err := modifyResourceAttributes(s.lbAttrResource(ctx, arn, accountID, op), pairs, raw)
	if err != nil {
		return nil, err
	}
	return &elbv2.ModifyLoadBalancerAttributesOutput{Attributes: sdkAttrsOut(submitted, mkLBAttr)}, nil
}

func (s *ELBv2ServiceImpl) DescribeLoadBalancerAttributes(ctx context.Context, input *elbv2.DescribeLoadBalancerAttributesInput, accountID string) (*elbv2.DescribeLoadBalancerAttributesOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	const op = "DescribeLoadBalancerAttributes"
	arn := aws.StringValue(input.LoadBalancerArn)
	pairs, err := describeResourceAttributes(s.lbAttrResource(ctx, arn, accountID, op))
	if err != nil {
		return nil, err
	}
	return &elbv2.DescribeLoadBalancerAttributesOutput{Attributes: sdkAttrsOut(pairs, mkLBAttr)}, nil
}
