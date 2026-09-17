package handlers_ec2_tags

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/arn"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	handlers_ec2_instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go/jetstream"
)

// Ensure TagsServiceImpl implements TagsService.
var _ TagsService = (*TagsServiceImpl)(nil)

// Ensure TagsServiceImpl can project instance record tags into the store.
var _ handlers_ec2_instance.InstanceTagWriter = (*TagsServiceImpl)(nil)

// Ensure TagsServiceImpl can both project and clear vpc/subnet/sg/eni record tags.
var _ handlers_ec2_vpc.CentralTagStore = (*TagsServiceImpl)(nil)

// TagsServiceImpl implements TagsService over a JetStream KV bucket, one entry
// per resource, scoped by account in the key.
//
// The bucket replaced a per-resource S3 object because a tag write is a
// read-modify-write of the whole set and the handler answers on a cluster-wide
// queue group: only a compare-and-swap keeps two nodes from losing each other's
// keys. The object store is still held to read resources written before the
// move; nothing writes to it.
type TagsServiceImpl struct {
	config *config.Config
	store  objectstore.ObjectStore
	kv     jetstream.KeyValue
}

// NewTagsServiceImpl creates a new tags service implementation.
func NewTagsServiceImpl(cfg *config.Config, kv jetstream.KeyValue) *TagsServiceImpl {
	store := objectstore.NewS3ObjectStoreFromConfig(
		cfg.Predastore.Host,
		cfg.Predastore.Region,
		cfg.Predastore.AccessKey,
		cfg.Predastore.SecretKey,
	)

	return &TagsServiceImpl{
		config: cfg,
		store:  store,
		kv:     kv,
	}
}

// NewTagsServiceImplWithStore creates a tags service with a custom ObjectStore (for testing).
func NewTagsServiceImplWithStore(cfg *config.Config, store objectstore.ObjectStore, kv jetstream.KeyValue) *TagsServiceImpl {
	return &TagsServiceImpl{
		config: cfg,
		store:  store,
		kv:     kv,
	}
}

// describeTagsResourceType is the resource-type filter value DescribeTags
// reports. An unrecognised prefix keeps emitting "unknown" so the filter's wire
// behaviour is unchanged; authorization treats it as unresolvable instead.
func describeTagsResourceType(resourceID string) string {
	kind, ok := arn.EC2TypeForID(resourceID)
	if !ok {
		return "unknown"
	}
	return string(kind)
}

// getTagsKey returns the S3 key for storing tags for a resource, scoped by account.
func getTagsKey(accountID, resourceID string) string {
	return "tags/" + accountID + "/" + resourceID + ".json"
}

// getTagsPrefix returns the S3 prefix for listing all tags for an account.
func getTagsPrefix(accountID string) string {
	return "tags/" + accountID + "/"
}

// decodeTags decodes a stored tag map into a non-nil map, so a stored JSON
// "null" (which Unmarshal leaves untouched without error) yields an empty map
// rather than a nil one a caller would panic on when indexing.
func decodeTags(data []byte) (map[string]string, error) {
	tags := make(map[string]string)
	if len(data) == 0 {
		return tags, nil
	}
	if err := json.Unmarshal(data, &tags); err != nil {
		return nil, err
	}
	if tags == nil {
		tags = make(map[string]string)
	}
	return tags, nil
}

func encodeTags(tags map[string]string) ([]byte, error) {
	if tags == nil {
		tags = make(map[string]string)
	}
	return json.Marshal(tags)
}

// legacyTags reads a resource's tags from the object store the tag index used
// before it moved to KV. An absent object means the resource was never written
// there, which is the ordinary case for anything tagged since.
func (s *TagsServiceImpl) legacyTags(ctx context.Context, accountID, resourceID string) (map[string]string, error) {
	if s.store == nil {
		return make(map[string]string), nil
	}

	result, err := s.store.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.config.Predastore.Bucket),
		Key:    aws.String(getTagsKey(accountID, resourceID)),
	})
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return make(map[string]string), nil
		}
		return nil, err
	}
	defer result.Body.Close()

	data, err := io.ReadAll(result.Body)
	if err != nil {
		return nil, err
	}
	return decodeTags(data)
}

// deleteLegacyTags removes a resource's pre-move object. Without it a resource
// whose KV entry is deleted would have its old tags reappear from the fallback
// read on the next describe.
func (s *TagsServiceImpl) deleteLegacyTags(ctx context.Context, accountID, resourceID string) error {
	if s.store == nil {
		return nil
	}
	_, err := s.store.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.config.Predastore.Bucket),
		Key:    aws.String(getTagsKey(accountID, resourceID)),
	})
	if err != nil && !objectstore.IsNoSuchKeyError(err) {
		return err
	}
	return nil
}

// getResourceTags returns a resource's tags, preferring the KV entry and falling
// back to the pre-move object for a resource not yet written since.
func (s *TagsServiceImpl) getResourceTags(ctx context.Context, accountID, resourceID string) (map[string]string, error) {
	tags, found, err := s.readKVTags(ctx, accountID, resourceID)
	if err != nil {
		return nil, err
	}
	if found {
		return tags, nil
	}
	return s.legacyTags(ctx, accountID, resourceID)
}

// PutResourceTags overwrites the stored tag set for a resource. Used to
// project an instance record's tags (the source of truth) into the central
// store so describe-tags agrees with describe-instances.
func (s *TagsServiceImpl) PutResourceTags(ctx context.Context, accountID, resourceID string, tags map[string]string) error {
	return s.putTags(ctx, accountID, resourceID, tags)
}

// DeleteAllTags removes the stored tags for a resource, so describe-tags stops
// reporting a resource that is gone. Instance terminate uses it while the
// terminated record keeps its own tags until TTL; the vpc delete paths use it
// to retire the entry outright. Idempotent: an absent entry is not an error.
func (s *TagsServiceImpl) DeleteAllTags(ctx context.Context, accountID, resourceID string) error {
	if err := s.deleteTagsEntry(ctx, accountID, resourceID); err != nil {
		return err
	}
	return s.deleteLegacyTags(ctx, accountID, resourceID)
}

// CreateTags adds or overwrites tags for the specified resources.
func (s *TagsServiceImpl) CreateTags(ctx context.Context, input *ec2.CreateTagsInput, accountID string) (*ec2.CreateTagsOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	if len(input.Resources) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	if len(input.Tags) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	slog.InfoContext(ctx, "CreateTags request", "resources", len(input.Resources), "tags", len(input.Tags))

	for _, resourceID := range input.Resources {
		if resourceID == nil {
			continue
		}

		// The merge runs inside the CAS attempt, so a writer that loses the race
		// re-reads the other writer's keys and adds its own on top of them.
		err := s.mutateTags(ctx, accountID, *resourceID, func(tags map[string]string) {
			for _, tag := range input.Tags {
				if tag.Key != nil && tag.Value != nil {
					tags[*tag.Key] = *tag.Value
				}
			}
		})
		if err != nil {
			slog.ErrorContext(ctx, "CreateTags failed to save tags", "resourceId", *resourceID, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}

		slog.InfoContext(ctx, "CreateTags applied", "resourceId", *resourceID, "tagCount", len(input.Tags))
	}

	return &ec2.CreateTagsOutput{}, nil
}

// taggedResourceIDs lists every resource the account has tags for, across both
// the KV bucket and the objects written before the store moved.
//
// The two are unioned rather than read in preference order because a resource
// migrated by a write since the move exists in both, and a resource never
// touched since exists only in the object store. Sorting keeps a describe's
// order stable between calls, which the KV listing does not guarantee on its
// own.
func (s *TagsServiceImpl) taggedResourceIDs(ctx context.Context, accountID string) ([]string, error) {
	seen := make(map[string]struct{})

	keys, err := kvutil.Keys(ctx, s.kv)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, fmt.Errorf("list tag keys: %w", err)
	}
	for _, key := range keys {
		if resourceID, ok := resourceIDFromKVKey(key, accountID); ok {
			seen[resourceID] = struct{}{}
		}
	}

	if s.store != nil {
		objects, _, lerr := objectstore.ListAll(ctx, s.store, &s3.ListObjectsV2Input{
			Bucket: aws.String(s.config.Predastore.Bucket),
			Prefix: aws.String(getTagsPrefix(accountID)),
		})
		if lerr != nil {
			return nil, fmt.Errorf("list legacy tag objects: %w", lerr)
		}
		for _, obj := range objects {
			if obj == nil || obj.Key == nil {
				continue
			}
			resourceID := strings.TrimPrefix(*obj.Key, getTagsPrefix(accountID))
			resourceID = strings.TrimSuffix(resourceID, ".json")
			if resourceID == "" {
				continue
			}
			seen[resourceID] = struct{}{}
		}
	}

	return slices.Sorted(maps.Keys(seen)), nil
}

var describeTagsValidFilters = map[string]bool{
	"resource-id":   true,
	"resource-type": true,
	"key":           true,
	"value":         true,
}

// DescribeTags returns tags matching the specified filters.
func (s *TagsServiceImpl) DescribeTags(ctx context.Context, input *ec2.DescribeTagsInput, accountID string) (*ec2.DescribeTagsOutput, error) {
	var filters map[string][]string
	if input != nil {
		var err error
		filters, err = filterutil.ParseFilters(input.Filters, describeTagsValidFilters)
		if err != nil {
			slog.WarnContext(ctx, "DescribeTags: invalid filter", "err", err)
			return nil, err
		}
	}

	slog.InfoContext(ctx, "DescribeTags request")

	var tags []*ec2.TagDescription

	resourceIDs, err := s.taggedResourceIDs(ctx, accountID)
	if err != nil {
		slog.ErrorContext(ctx, "DescribeTags failed to list tagged resources", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	for _, resourceID := range resourceIDs {
		resourceType := describeTagsResourceType(resourceID)

		if !filterutil.MatchesAny(filters["resource-id"], resourceID) {
			continue
		}
		if !filterutil.MatchesAny(filters["resource-type"], resourceType) {
			continue
		}

		// Get tags for this resource
		resourceTags, err := s.getResourceTags(ctx, accountID, resourceID)
		if err != nil {
			slog.WarnContext(ctx, "DescribeTags failed to get tags", "resourceId", resourceID, "err", err)
			continue
		}

		for key, value := range resourceTags {
			if !filterutil.MatchesAny(filters["key"], key) {
				continue
			}
			if !filterutil.MatchesAny(filters["value"], value) {
				continue
			}

			tags = append(tags, &ec2.TagDescription{
				ResourceId:   aws.String(resourceID),
				ResourceType: aws.String(resourceType),
				Key:          aws.String(key),
				Value:        aws.String(value),
			})
		}
	}

	slog.InfoContext(ctx, "DescribeTags completed", "count", len(tags))

	return &ec2.DescribeTagsOutput{
		Tags: tags,
	}, nil
}

// DeleteTags removes tags from the specified resources.
func (s *TagsServiceImpl) DeleteTags(ctx context.Context, input *ec2.DeleteTagsInput, accountID string) (*ec2.DeleteTagsOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	if len(input.Resources) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	slog.InfoContext(ctx, "DeleteTags request", "resources", len(input.Resources), "tags", len(input.Tags))

	remove := utils.RemoveTagsMut(input)

	for _, resourceID := range input.Resources {
		if resourceID == nil {
			continue
		}

		var remaining int
		err := s.mutateTags(ctx, accountID, *resourceID, func(tags map[string]string) {
			remove(tags)
			remaining = len(tags)
		})
		if err != nil {
			slog.ErrorContext(ctx, "DeleteTags failed to save tags", "resourceId", *resourceID, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}

		slog.InfoContext(ctx, "DeleteTags applied", "resourceId", *resourceID, "remainingTags", remaining)
	}

	return &ec2.DeleteTagsOutput{}, nil
}
