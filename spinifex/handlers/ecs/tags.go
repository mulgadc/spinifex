package handlers_ecs

import (
	"context"
	"errors"
	"maps"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go/jetstream"
)

// ecsResourceKind identifies which stored record kind an ARN refers to.
type ecsResourceKind int

const (
	ecsResourceCluster ecsResourceKind = iota
	ecsResourceTaskDefinition
	ecsResourceService
	ecsResourceTask
	ecsResourceContainerInstance
)

// parseResourceARN splits an ECS resource ARN into its kind, owning cluster
// (empty for cluster/task-definition, which are flat), and short id/name.
// Cluster and task-definition ARNs are flat (.../cluster/{name},
// .../task-definition/{family}:{rev}); service/task/container-instance ARNs
// embed the owning cluster (.../service/{cluster}/{name}).
func parseResourceARN(resourceARN string) (kind ecsResourceKind, cluster, id string, err error) {
	// SplitN(..., 6) keeps the resource segment intact even though a
	// task-definition id ("family:1") itself contains a colon.
	parts := strings.SplitN(resourceARN, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" {
		return 0, "", "", errors.New(awserrors.ErrorECSInvalidParameter)
	}
	rtype, rest, ok := strings.Cut(parts[5], "/")
	if !ok || rtype == "" || rest == "" {
		return 0, "", "", errors.New(awserrors.ErrorECSInvalidParameter)
	}

	switch rtype {
	case "cluster":
		return ecsResourceCluster, rest, rest, nil
	case "task-definition":
		return ecsResourceTaskDefinition, "", rest, nil
	case "service", "task", "container-instance":
		clusterName, name, ok := strings.Cut(rest, "/")
		if !ok || clusterName == "" || name == "" {
			return 0, "", "", errors.New(awserrors.ErrorECSInvalidParameter)
		}
		switch rtype {
		case "service":
			return ecsResourceService, clusterName, name, nil
		case "task":
			return ecsResourceTask, clusterName, name, nil
		default:
			return ecsResourceContainerInstance, clusterName, name, nil
		}
	default:
		return 0, "", "", errors.New(awserrors.ErrorECSInvalidParameter)
	}
}

// resourceTags reads the tag map stored on the ARN-identified resource.
func (s *Service) resourceTags(ctx context.Context, kv jetstream.KeyValue, kind ecsResourceKind, cluster, id string) (map[string]string, error) {
	switch kind {
	case ecsResourceCluster:
		var rec ClusterRecord
		found, err := getJSON(ctx, kv, ClusterMetaKey(id), &rec)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, errors.New(awserrors.ErrorECSClusterNotFound)
		}
		return rec.Tags, nil
	case ecsResourceTaskDefinition:
		family, rev := parseTaskDefRef(id)
		if family == "" || rev == 0 {
			return nil, errors.New(awserrors.ErrorECSInvalidParameter)
		}
		var rec TaskDefRecord
		found, err := getJSON(ctx, kv, TaskDefRevKey(family, rev), &rec)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, errors.New(awserrors.ErrorECSInvalidParameter)
		}
		return rec.Tags, nil
	case ecsResourceService:
		var rec ServiceRecord
		found, err := getJSON(ctx, kv, ServiceKey(cluster, id), &rec)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, errors.New(awserrors.ErrorECSServiceNotFound)
		}
		return rec.Tags, nil
	case ecsResourceTask:
		var rec TaskRecord
		found, err := getJSON(ctx, kv, TaskKey(cluster, id), &rec)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, errors.New(awserrors.ErrorECSInvalidParameter)
		}
		return rec.Tags, nil
	case ecsResourceContainerInstance:
		var rec InstanceRecord
		found, err := getJSON(ctx, kv, InstanceKey(cluster, id), &rec)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, errors.New(awserrors.ErrorECSInvalidParameter)
		}
		return rec.Tags, nil
	default:
		return nil, errors.New(awserrors.ErrorECSInvalidParameter)
	}
}

// mutateResourceTags reads the ARN-identified resource, applies mutate to its
// tag map, and re-puts the record. Used by TagResource (merge) and
// UntagResource (delete).
func (s *Service) mutateResourceTags(ctx context.Context, kv jetstream.KeyValue, kind ecsResourceKind, cluster, id string, mutate func(map[string]string) map[string]string) error {
	switch kind {
	case ecsResourceCluster:
		var rec ClusterRecord
		found, err := getJSON(ctx, kv, ClusterMetaKey(id), &rec)
		if err != nil {
			return err
		}
		if !found {
			return errors.New(awserrors.ErrorECSClusterNotFound)
		}
		rec.Tags = mutate(rec.Tags)
		return putJSON(ctx, kv, ClusterMetaKey(id), &rec)
	case ecsResourceTaskDefinition:
		family, rev := parseTaskDefRef(id)
		if family == "" || rev == 0 {
			return errors.New(awserrors.ErrorECSInvalidParameter)
		}
		var rec TaskDefRecord
		found, err := getJSON(ctx, kv, TaskDefRevKey(family, rev), &rec)
		if err != nil {
			return err
		}
		if !found {
			return errors.New(awserrors.ErrorECSInvalidParameter)
		}
		rec.Tags = mutate(rec.Tags)
		return putJSON(ctx, kv, TaskDefRevKey(family, rev), &rec)
	case ecsResourceService:
		var rec ServiceRecord
		found, err := getJSON(ctx, kv, ServiceKey(cluster, id), &rec)
		if err != nil {
			return err
		}
		if !found {
			return errors.New(awserrors.ErrorECSServiceNotFound)
		}
		rec.Tags = mutate(rec.Tags)
		return putJSON(ctx, kv, ServiceKey(cluster, id), &rec)
	case ecsResourceTask:
		var rec TaskRecord
		found, err := getJSON(ctx, kv, TaskKey(cluster, id), &rec)
		if err != nil {
			return err
		}
		if !found {
			return errors.New(awserrors.ErrorECSInvalidParameter)
		}
		rec.Tags = mutate(rec.Tags)
		return putJSON(ctx, kv, TaskKey(cluster, id), &rec)
	case ecsResourceContainerInstance:
		var rec InstanceRecord
		found, err := getJSON(ctx, kv, InstanceKey(cluster, id), &rec)
		if err != nil {
			return err
		}
		if !found {
			return errors.New(awserrors.ErrorECSInvalidParameter)
		}
		rec.Tags = mutate(rec.Tags)
		return putJSON(ctx, kv, InstanceKey(cluster, id), &rec)
	default:
		return errors.New(awserrors.ErrorECSInvalidParameter)
	}
}

// ListTagsForResource returns the tags stored on the ARN-identified resource.
func (s *Service) ListTagsForResource(ctx context.Context, input *ecs.ListTagsForResourceInput, accountID string) (*ecs.ListTagsForResourceOutput, error) {
	if input == nil || aws.StringValue(input.ResourceArn) == "" {
		return nil, errors.New(awserrors.ErrorECSInvalidParameter)
	}
	kind, cluster, id, err := parseResourceARN(aws.StringValue(input.ResourceArn))
	if err != nil {
		return nil, err
	}
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	tags, err := s.resourceTags(ctx, kv, kind, cluster, id)
	if err != nil {
		return nil, err
	}
	return &ecs.ListTagsForResourceOutput{Tags: tagsToAWS(tags)}, nil
}

// TagResource merges the supplied tags onto the ARN-identified resource.
func (s *Service) TagResource(ctx context.Context, input *ecs.TagResourceInput, accountID string) (*ecs.TagResourceOutput, error) {
	if input == nil || aws.StringValue(input.ResourceArn) == "" {
		return nil, errors.New(awserrors.ErrorECSInvalidParameter)
	}
	kind, cluster, id, err := parseResourceARN(aws.StringValue(input.ResourceArn))
	if err != nil {
		return nil, err
	}
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	add := tagsToMap(input.Tags)
	err = s.mutateResourceTags(ctx, kv, kind, cluster, id, func(existing map[string]string) map[string]string {
		if existing == nil {
			existing = map[string]string{}
		}
		maps.Copy(existing, add)
		return existing
	})
	if err != nil {
		return nil, err
	}
	return &ecs.TagResourceOutput{}, nil
}

// UntagResource deletes the named tag keys from the ARN-identified resource.
func (s *Service) UntagResource(ctx context.Context, input *ecs.UntagResourceInput, accountID string) (*ecs.UntagResourceOutput, error) {
	if input == nil || aws.StringValue(input.ResourceArn) == "" {
		return nil, errors.New(awserrors.ErrorECSInvalidParameter)
	}
	kind, cluster, id, err := parseResourceARN(aws.StringValue(input.ResourceArn))
	if err != nil {
		return nil, err
	}
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	keys := awsStringSlice(input.TagKeys)
	err = s.mutateResourceTags(ctx, kv, kind, cluster, id, func(existing map[string]string) map[string]string {
		for _, k := range keys {
			delete(existing, k)
		}
		return existing
	})
	if err != nil {
		return nil, err
	}
	return &ecs.UntagResourceOutput{}, nil
}
