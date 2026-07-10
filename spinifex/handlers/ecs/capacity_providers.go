package handlers_ecs

import (
	"context"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// PutClusterCapacityProviders persists the cluster's capacity-provider names
// and default strategy. Accepted and stored only: no scheduler coupling, no
// scale loop (a separate follow-on binds this to an ASG primitive).
func (s *Service) PutClusterCapacityProviders(_ context.Context, input *ecs.PutClusterCapacityProvidersInput, accountID string) (*ecs.PutClusterCapacityProvidersOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorECSInvalidParameter)
	}
	cluster := clusterShortName(aws.StringValue(input.Cluster))
	kv, err := s.bucket(accountID)
	if err != nil {
		return nil, err
	}
	var rec ClusterRecord
	found, err := getJSON(kv, ClusterMetaKey(cluster), &rec)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New(awserrors.ErrorECSClusterNotFound)
	}

	rec.CapacityProviders = awsStringSlice(input.CapacityProviders)
	rec.DefaultCapacityProviderStrategy = strategyFromAWS(input.DefaultCapacityProviderStrategy)
	if err := putJSON(kv, ClusterMetaKey(cluster), &rec); err != nil {
		return nil, err
	}
	return &ecs.PutClusterCapacityProvidersOutput{Cluster: rec.toAWS()}, nil
}

// CreateCapacityProvider persists a new capacity provider. Idempotent:
// re-creating an existing name returns the existing record.
func (s *Service) CreateCapacityProvider(_ context.Context, input *ecs.CreateCapacityProviderInput, accountID string) (*ecs.CreateCapacityProviderOutput, error) {
	if input == nil || aws.StringValue(input.Name) == "" || input.AutoScalingGroupProvider == nil ||
		aws.StringValue(input.AutoScalingGroupProvider.AutoScalingGroupArn) == "" {
		return nil, errors.New(awserrors.ErrorECSInvalidParameter)
	}
	name := aws.StringValue(input.Name)
	kv, err := s.bucket(accountID)
	if err != nil {
		return nil, err
	}

	var existing CapacityProviderRecord
	found, err := getJSON(kv, CapacityProviderKey(name), &existing)
	if err != nil {
		return nil, err
	}
	if found {
		return &ecs.CreateCapacityProviderOutput{CapacityProvider: existing.toAWS()}, nil
	}

	rec := CapacityProviderRecord{
		Name:                     name,
		ARN:                      CapacityProviderARN(s.region, accountID, name),
		Status:                   CapacityProviderStatusActive,
		AutoScalingGroupProvider: autoScalingGroupProviderFromAWS(input.AutoScalingGroupProvider),
		Tags:                     tagsToMap(input.Tags),
		CreatedAt:                time.Now().UTC(),
	}
	if err := putJSON(kv, CapacityProviderKey(name), &rec); err != nil {
		return nil, err
	}
	return &ecs.CreateCapacityProviderOutput{CapacityProvider: rec.toAWS()}, nil
}

// DescribeCapacityProviders returns the named providers, or every provider in
// the account when none are named. Unknown names surface as failures.
func (s *Service) DescribeCapacityProviders(_ context.Context, input *ecs.DescribeCapacityProvidersInput, accountID string) (*ecs.DescribeCapacityProvidersOutput, error) {
	kv, err := s.bucket(accountID)
	if err != nil {
		return nil, err
	}
	out := &ecs.DescribeCapacityProvidersOutput{}

	names := awsStringSlice(input.CapacityProviders)
	if len(names) == 0 {
		keys, kerr := keysWithPrefix(kv, CapacityProvidersPrefix())
		if kerr != nil {
			return nil, kerr
		}
		for _, k := range keys {
			var rec CapacityProviderRecord
			ok, gerr := getJSON(kv, k, &rec)
			if gerr != nil {
				return nil, gerr
			}
			if ok {
				out.CapacityProviders = append(out.CapacityProviders, rec.toAWS())
			}
		}
		return out, nil
	}

	for _, ref := range names {
		name := capacityProviderShortName(ref)
		var rec CapacityProviderRecord
		ok, gerr := getJSON(kv, CapacityProviderKey(name), &rec)
		if gerr != nil {
			return nil, gerr
		}
		if ok {
			out.CapacityProviders = append(out.CapacityProviders, rec.toAWS())
		} else {
			out.Failures = append(out.Failures, &ecs.Failure{Arn: aws.String(ref), Reason: aws.String("MISSING")})
		}
	}
	return out, nil
}

// DeleteCapacityProvider removes a capacity provider record. Clusters that
// still reference the name by CapacityProviders are left untouched (v1 does
// not enforce referential integrity here).
func (s *Service) DeleteCapacityProvider(_ context.Context, input *ecs.DeleteCapacityProviderInput, accountID string) (*ecs.DeleteCapacityProviderOutput, error) {
	if input == nil || aws.StringValue(input.CapacityProvider) == "" {
		return nil, errors.New(awserrors.ErrorECSInvalidParameter)
	}
	name := capacityProviderShortName(aws.StringValue(input.CapacityProvider))
	kv, err := s.bucket(accountID)
	if err != nil {
		return nil, err
	}
	var rec CapacityProviderRecord
	found, err := getJSON(kv, CapacityProviderKey(name), &rec)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New(awserrors.ErrorECSInvalidParameter)
	}
	if err := kv.Delete(CapacityProviderKey(name)); err != nil {
		return nil, err
	}
	return &ecs.DeleteCapacityProviderOutput{CapacityProvider: rec.toAWS()}, nil
}

// --- Conversions ---

func (r *CapacityProviderRecord) toAWS() *ecs.CapacityProvider {
	return &ecs.CapacityProvider{
		Name:                     aws.String(r.Name),
		CapacityProviderArn:      aws.String(r.ARN),
		Status:                   aws.String(r.Status),
		AutoScalingGroupProvider: r.AutoScalingGroupProvider.toAWS(),
		Tags:                     tagsToAWS(r.Tags),
	}
}

func autoScalingGroupProviderFromAWS(in *ecs.AutoScalingGroupProvider) AutoScalingGroupProviderRecord {
	if in == nil {
		return AutoScalingGroupProviderRecord{}
	}
	rec := AutoScalingGroupProviderRecord{
		AutoScalingGroupARN:          aws.StringValue(in.AutoScalingGroupArn),
		ManagedTerminationProtection: aws.StringValue(in.ManagedTerminationProtection),
	}
	if in.ManagedScaling != nil {
		rec.ManagedScaling = ManagedScalingRecord{
			Status:                 aws.StringValue(in.ManagedScaling.Status),
			TargetCapacity:         int(aws.Int64Value(in.ManagedScaling.TargetCapacity)),
			MinimumScalingStepSize: int(aws.Int64Value(in.ManagedScaling.MinimumScalingStepSize)),
			MaximumScalingStepSize: int(aws.Int64Value(in.ManagedScaling.MaximumScalingStepSize)),
			InstanceWarmupPeriod:   int(aws.Int64Value(in.ManagedScaling.InstanceWarmupPeriod)),
		}
	}
	return rec
}

func (r AutoScalingGroupProviderRecord) toAWS() *ecs.AutoScalingGroupProvider {
	out := &ecs.AutoScalingGroupProvider{AutoScalingGroupArn: aws.String(r.AutoScalingGroupARN)}
	if r.ManagedTerminationProtection != "" {
		out.ManagedTerminationProtection = aws.String(r.ManagedTerminationProtection)
	}
	if (r.ManagedScaling != ManagedScalingRecord{}) {
		ms := &ecs.ManagedScaling{}
		if r.ManagedScaling.Status != "" {
			ms.Status = aws.String(r.ManagedScaling.Status)
		}
		if r.ManagedScaling.TargetCapacity > 0 {
			ms.TargetCapacity = aws.Int64(int64(r.ManagedScaling.TargetCapacity))
		}
		if r.ManagedScaling.MinimumScalingStepSize > 0 {
			ms.MinimumScalingStepSize = aws.Int64(int64(r.ManagedScaling.MinimumScalingStepSize))
		}
		if r.ManagedScaling.MaximumScalingStepSize > 0 {
			ms.MaximumScalingStepSize = aws.Int64(int64(r.ManagedScaling.MaximumScalingStepSize))
		}
		if r.ManagedScaling.InstanceWarmupPeriod > 0 {
			ms.InstanceWarmupPeriod = aws.Int64(int64(r.ManagedScaling.InstanceWarmupPeriod))
		}
		out.ManagedScaling = ms
	}
	return out
}

// strategyFromAWS maps the SDK capacity-provider strategy list to the
// persisted subset, dropping nil entries.
func strategyFromAWS(in []*ecs.CapacityProviderStrategyItem) []CapacityProviderStrategyItem {
	out := make([]CapacityProviderStrategyItem, 0, len(in))
	for _, item := range in {
		if item == nil {
			continue
		}
		out = append(out, CapacityProviderStrategyItem{
			Provider: aws.StringValue(item.CapacityProvider),
			Weight:   int(aws.Int64Value(item.Weight)),
			Base:     int(aws.Int64Value(item.Base)),
		})
	}
	return out
}

func (item CapacityProviderStrategyItem) toAWS() *ecs.CapacityProviderStrategyItem {
	out := &ecs.CapacityProviderStrategyItem{CapacityProvider: aws.String(item.Provider)}
	if item.Weight > 0 {
		out.Weight = aws.Int64(int64(item.Weight))
	}
	if item.Base > 0 {
		out.Base = aws.Int64(int64(item.Base))
	}
	return out
}
