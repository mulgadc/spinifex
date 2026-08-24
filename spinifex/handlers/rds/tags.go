package handlers_rds

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go/jetstream"
)

// AWS's own tag limits. The reserved prefix is refused rather than dropped: a
// silently discarded aws: tag would read back as absent on the next apply.
const (
	maxTagsPerResource = 50
	maxTagKeyLen       = 128
	maxTagValueLen     = 256
	reservedTagPrefix  = "aws:"
)

// A tag write is a read-modify-write, so a concurrent add and remove race on
// the same record. Each round has exactly one winner, so the bound is the
// number of writers that can contend for one resource, not a timing guess.
const tagWriteAttempts = 16

// Implemented by every record whose ARN kind is registered below. Keeping the
// tag map behind an interface is what lets one mutate path serve every
// resource type without a type switch per action.
type TaggedRecord interface {
	GetTags() map[string]string
	SetTags(tags map[string]string)
}

// How a resource kind's tags are reached: its key in the account bucket, the
// record they live on, and the fault its absence raises.
type taggableResource struct {
	key       func(identifier string) string
	newRecord func() TaggedRecord
	notFound  string
}

// One entry per resource kind, registered by the phase that creates the
// resource. A default parameter group is deliberately absent: it has no stored
// record, so a tag written to it would have nowhere to live and would read back
// as absent on the next apply.
var taggableResources = map[ResourceKind]taggableResource{
	ResourceKindDBInstance: {
		key:       DBInstanceKey,
		newRecord: func() TaggedRecord { return &DBInstanceRecord{} },
		notFound:  awserrors.ErrorDBInstanceNotFound,
	},
	ResourceKindDBSnapshot: {
		key:       DBSnapshotKey,
		newRecord: func() TaggedRecord { return &DBSnapshotRecord{} },
		notFound:  awserrors.ErrorDBSnapshotNotFound,
	},
	ResourceKindDBSubnetGroup: {
		key:       DBSubnetGroupKey,
		newRecord: func() TaggedRecord { return &DBSubnetGroupRecord{} },
		notFound:  awserrors.ErrorDBSubnetGroupNotFound,
	},
	ResourceKindDBParameterGroup: {
		key:       DBParameterGroupMetaKey,
		newRecord: func() TaggedRecord { return &DBParameterGroupRecord{} },
		notFound:  awserrors.ErrorDBParameterGroupNotFound,
	},
}

func (s *Service) ListTagsForResource(ctx context.Context, input *rds.ListTagsForResourceInput, accountID string) (*rds.ListTagsForResourceOutput, error) {
	if input == nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "empty request")
	}
	resource, parsed, err := s.resolveTaggable(aws.StringValue(input.ResourceName), accountID)
	if err != nil {
		return nil, err
	}
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	rec, _, err := readTagged(ctx, kv, resource, parsed.Identifier)
	if err != nil {
		return nil, err
	}
	return &rds.ListTagsForResourceOutput{TagList: tagsToAWS(rec.GetTags())}, nil
}

// Adding a key that is already present overwrites it, so a repeated Terraform
// apply converges instead of failing on the second run.
func (s *Service) AddTagsToResource(ctx context.Context, input *rds.AddTagsToResourceInput, accountID string) (*rds.AddTagsToResourceOutput, error) {
	if input == nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "empty request")
	}
	resource, parsed, err := s.resolveTaggable(aws.StringValue(input.ResourceName), accountID)
	if err != nil {
		return nil, err
	}
	add, err := validateTags(input.Tags)
	if err != nil {
		return nil, err
	}
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}

	err = mutateTags(ctx, kv, resource, parsed.Identifier, func(existing map[string]string) (map[string]string, error) {
		merged := make(map[string]string, len(existing)+len(add))
		maps.Copy(merged, existing)
		maps.Copy(merged, add)
		// Checked against the merge rather than the request: 40 existing tags plus
		// 40 new ones is over the limit even though neither side is.
		if len(merged) > maxTagsPerResource {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
				"a resource may carry at most %d tags; this request would leave %d", maxTagsPerResource, len(merged))
		}
		return merged, nil
	})
	if err != nil {
		return nil, err
	}
	return &rds.AddTagsToResourceOutput{}, nil
}

// Removing a key that is not present is a success, matching AWS: a retried
// destroy must not fail on the tags it already removed.
func (s *Service) RemoveTagsFromResource(ctx context.Context, input *rds.RemoveTagsFromResourceInput, accountID string) (*rds.RemoveTagsFromResourceOutput, error) {
	if input == nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "empty request")
	}
	resource, parsed, err := s.resolveTaggable(aws.StringValue(input.ResourceName), accountID)
	if err != nil {
		return nil, err
	}
	keys := aws.StringValueSlice(input.TagKeys)
	for _, key := range keys {
		if err := validateTagKey(key); err != nil {
			return nil, err
		}
	}
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}

	err = mutateTags(ctx, kv, resource, parsed.Identifier, func(existing map[string]string) (map[string]string, error) {
		remaining := maps.Clone(existing)
		for _, key := range keys {
			delete(remaining, key)
		}
		return remaining, nil
	})
	if err != nil {
		return nil, err
	}
	return &rds.RemoveTagsFromResourceOutput{}, nil
}

// Parses the ARN and looks its kind up in the registry. A well-formed ARN for a
// kind no phase has registered is rejected rather than answered with an empty
// tag list, which would be indistinguishable from an untagged resource.
func (s *Service) resolveTaggable(resourceName, accountID string) (taggableResource, ParsedARN, error) {
	parsed, err := ParseARN(resourceName, s.region, accountID)
	if err != nil {
		return taggableResource{}, ParsedARN{}, err
	}
	resource, ok := taggableResources[parsed.Kind]
	if !ok {
		return taggableResource{}, ParsedARN{}, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"tagging %s resources is not supported yet", parsed.Kind)
	}
	return resource, parsed, nil
}

// The record plus its revision. A missing record raises the resource's own
// not-found fault, so an ARN that parses but names nothing is distinguishable
// from one that does not parse.
func readTagged(ctx context.Context, kv jetstream.KeyValue, resource taggableResource, identifier string) (TaggedRecord, uint64, error) {
	rec := resource.newRecord()
	rev, found, err := getJSONRevision(ctx, kv, resource.key(identifier), rec)
	if err != nil {
		return nil, 0, err
	}
	if !found {
		return nil, 0, errors.New(resource.notFound)
	}
	return rec, rev, nil
}

// Applies mutate to the record's tags under CAS. Losing the CAS means another
// tag write landed first, so the mutation is replayed against its result rather
// than overwriting it.
func mutateTags(ctx context.Context, kv jetstream.KeyValue, resource taggableResource, identifier string, mutate func(map[string]string) (map[string]string, error)) error {
	key := resource.key(identifier)
	for range tagWriteAttempts {
		rec, rev, err := readTagged(ctx, kv, resource, identifier)
		if err != nil {
			return err
		}
		next, err := mutate(rec.GetTags())
		if err != nil {
			return err
		}
		rec.SetTags(next)

		err = updateJSON(ctx, kv, key, rev, rec)
		if err == nil {
			return nil
		}
		// A revision mismatch is a lost race with another writer, not a
		// duplicate: re-read the record and re-apply the tag mutation.
		if !errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
			return err
		}
	}
	return fmt.Errorf("rds: tag update on %s contended after %d attempts", key, tagWriteAttempts)
}

// The AWS tag rules, applied identically at create and at AddTagsToResource so
// a create cannot produce tags a later modify would reject. A key repeated
// within one request keeps its last value, as AWS does.
func validateTags(tags []*rds.Tag) (map[string]string, error) {
	if len(tags) > maxTagsPerResource {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"at most %d tags may be supplied, got %d", maxTagsPerResource, len(tags))
	}
	out := make(map[string]string, len(tags))
	for _, tag := range tags {
		if tag == nil {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "a tag entry is empty")
		}
		key := aws.StringValue(tag.Key)
		if err := validateTagKey(key); err != nil {
			return nil, err
		}
		value := aws.StringValue(tag.Value)
		if len(value) > maxTagValueLen {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
				"tag value for key %q may be at most %d characters", key, maxTagValueLen)
		}
		out[key] = value
	}
	return out, nil
}

func validateTagKey(key string) error {
	switch {
	case key == "":
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "a tag key may not be empty")
	case len(key) > maxTagKeyLen:
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"tag key %q may be at most %d characters", key, maxTagKeyLen)
	case strings.HasPrefix(strings.ToLower(key), reservedTagPrefix):
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"tag key %q uses the reserved %q prefix", key, reservedTagPrefix)
	}
	return nil
}

// Sorted by key so a describe and a list return the same order on every call,
// which is what keeps Terraform from reading a reshuffle as drift.
func tagsToAWS(tags map[string]string) []*rds.Tag {
	if len(tags) == 0 {
		return nil
	}
	out := make([]*rds.Tag, 0, len(tags))
	for _, key := range slices.Sorted(maps.Keys(tags)) {
		out = append(out, &rds.Tag{Key: aws.String(key), Value: aws.String(tags[key])})
	}
	return out
}
