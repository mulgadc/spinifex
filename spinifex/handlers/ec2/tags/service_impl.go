package handlers_ec2_tags

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	handlers_ec2_instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
)

// Ensure TagsServiceImpl implements TagsService
var _ TagsService = (*TagsServiceImpl)(nil)

// Ensure TagsServiceImpl can project instance record tags into the store
var _ handlers_ec2_instance.InstanceTagWriter = (*TagsServiceImpl)(nil)

// TagsServiceImpl implements TagsService with S3-backed storage.
// Tags are stored per-account in S3 (tags/{accountID}/{resourceID}.json),
// so account scoping is enforced at the storage layer.
type TagsServiceImpl struct {
	config *config.Config
	store  objectstore.ObjectStore
	mutex  sync.RWMutex
}

// NewTagsServiceImpl creates a new tags service implementation
func NewTagsServiceImpl(cfg *config.Config) *TagsServiceImpl {
	store := objectstore.NewS3ObjectStoreFromConfig(
		cfg.Predastore.Host,
		cfg.Predastore.Region,
		cfg.Predastore.AccessKey,
		cfg.Predastore.SecretKey,
	)

	return &TagsServiceImpl{
		config: cfg,
		store:  store,
	}
}

// NewTagsServiceImplWithStore creates a tags service with a custom ObjectStore (for testing)
func NewTagsServiceImplWithStore(cfg *config.Config, store objectstore.ObjectStore) *TagsServiceImpl {
	return &TagsServiceImpl{
		config: cfg,
		store:  store,
	}
}

// getResourceType extracts resource type from resource ID prefix
func getResourceType(resourceID string) string {
	if strings.HasPrefix(resourceID, "i-") {
		return "instance"
	}
	if strings.HasPrefix(resourceID, "vol-") {
		return "volume"
	}
	if strings.HasPrefix(resourceID, "ami-") {
		return "image"
	}
	if strings.HasPrefix(resourceID, "snap-") {
		return "snapshot"
	}
	if strings.HasPrefix(resourceID, "vpc-") {
		return "vpc"
	}
	if strings.HasPrefix(resourceID, "subnet-") {
		return "subnet"
	}
	if strings.HasPrefix(resourceID, "sg-") {
		return "security-group"
	}
	if strings.HasPrefix(resourceID, "rtb-") {
		return "route-table"
	}
	if strings.HasPrefix(resourceID, "igw-") {
		return "internet-gateway"
	}
	if strings.HasPrefix(resourceID, "eigw-") {
		return "egress-only-internet-gateway"
	}
	if strings.HasPrefix(resourceID, "eni-") {
		return "network-interface"
	}
	if strings.HasPrefix(resourceID, "eipalloc-") {
		return "elastic-ip"
	}
	if strings.HasPrefix(resourceID, "nat-") {
		return "natgateway"
	}
	if strings.HasPrefix(resourceID, "key-") {
		return "key-pair"
	}
	if strings.HasPrefix(resourceID, "pg-") {
		return "placement-group"
	}
	return "unknown"
}

// getTagsKey returns the S3 key for storing tags for a resource, scoped by account
func getTagsKey(accountID, resourceID string) string {
	return "tags/" + accountID + "/" + resourceID + ".json"
}

// getTagsPrefix returns the S3 prefix for listing all tags for an account
func getTagsPrefix(accountID string) string {
	return "tags/" + accountID + "/"
}

// getResourceTags retrieves tags for a specific resource from S3
func (s *TagsServiceImpl) getResourceTags(accountID, resourceID string) (map[string]string, error) {
	key := getTagsKey(accountID, resourceID)

	result, err := s.store.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(s.config.Predastore.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return make(map[string]string), nil
		}
		return nil, err
	}
	defer result.Body.Close()

	var tags map[string]string
	if err := json.NewDecoder(result.Body).Decode(&tags); err != nil {
		return nil, err
	}

	return tags, nil
}

// putResourceTags stores tags for a specific resource in S3
func (s *TagsServiceImpl) putResourceTags(accountID, resourceID string, tags map[string]string) error {
	key := getTagsKey(accountID, resourceID)

	data, err := json.Marshal(tags)
	if err != nil {
		return err
	}

	_, err = s.store.PutObject(&s3.PutObjectInput{
		Bucket:      aws.String(s.config.Predastore.Bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/json"),
	})

	return err
}

// PutResourceTags overwrites the stored tag set for a resource. Used to
// project an instance record's tags (the source of truth) into the central
// store so describe-tags agrees with describe-instances.
func (s *TagsServiceImpl) PutResourceTags(accountID, resourceID string, tags map[string]string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.putResourceTags(accountID, resourceID, tags)
}

// DeleteAllTags removes the stored tag object for a resource. Used on
// instance terminate so describe-tags stops reporting the instance while the
// terminated record keeps its tags until TTL. Idempotent: a missing object
// is not an error.
func (s *TagsServiceImpl) DeleteAllTags(accountID, resourceID string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	_, err := s.store.DeleteObject(&s3.DeleteObjectInput{
		Bucket: aws.String(s.config.Predastore.Bucket),
		Key:    aws.String(getTagsKey(accountID, resourceID)),
	})
	if err != nil && !objectstore.IsNoSuchKeyError(err) {
		return err
	}
	return nil
}

// CreateTags adds or overwrites tags for the specified resources
func (s *TagsServiceImpl) CreateTags(input *ec2.CreateTagsInput, accountID string) (*ec2.CreateTagsOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	if len(input.Resources) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	if len(input.Tags) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	slog.Info("CreateTags request", "resources", len(input.Resources), "tags", len(input.Tags))

	for _, resourceID := range input.Resources {
		if resourceID == nil {
			continue
		}

		// Get existing tags
		existingTags, err := s.getResourceTags(accountID, *resourceID)
		if err != nil {
			slog.Error("CreateTags failed to get existing tags", "resourceId", *resourceID, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}

		// Add/update new tags
		for _, tag := range input.Tags {
			if tag.Key != nil && tag.Value != nil {
				existingTags[*tag.Key] = *tag.Value
			}
		}

		// Save tags
		if err := s.putResourceTags(accountID, *resourceID, existingTags); err != nil {
			slog.Error("CreateTags failed to save tags", "resourceId", *resourceID, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}

		slog.Info("CreateTags applied", "resourceId", *resourceID, "tagCount", len(existingTags))
	}

	return &ec2.CreateTagsOutput{}, nil
}

var describeTagsValidFilters = map[string]bool{
	"resource-id":   true,
	"resource-type": true,
	"key":           true,
	"value":         true,
}

// DescribeTags returns tags matching the specified filters
func (s *TagsServiceImpl) DescribeTags(input *ec2.DescribeTagsInput, accountID string) (*ec2.DescribeTagsOutput, error) {
	var filters map[string][]string
	if input != nil {
		var err error
		filters, err = filterutil.ParseFilters(input.Filters, describeTagsValidFilters)
		if err != nil {
			slog.Warn("DescribeTags: invalid filter", "err", err)
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
	}

	s.mutex.RLock()
	defer s.mutex.RUnlock()

	slog.Info("DescribeTags request")

	var tags []*ec2.TagDescription

	// List all tag files from S3 scoped to this account
	listResult, err := s.store.ListObjectsV2(&s3.ListObjectsV2Input{
		Bucket: aws.String(s.config.Predastore.Bucket),
		Prefix: aws.String(getTagsPrefix(accountID)),
	})
	if err != nil {
		slog.Error("DescribeTags failed to list objects", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Process each tag file
	for _, obj := range listResult.Contents {
		if obj.Key == nil {
			continue
		}

		// Extract resource ID from key (tags/{accountID}/i-xxx.json -> i-xxx)
		resourceID := strings.TrimPrefix(*obj.Key, getTagsPrefix(accountID))
		resourceID = strings.TrimSuffix(resourceID, ".json")
		resourceType := getResourceType(resourceID)

		if !filterutil.MatchesAny(filters["resource-id"], resourceID) {
			continue
		}
		if !filterutil.MatchesAny(filters["resource-type"], resourceType) {
			continue
		}

		// Get tags for this resource
		resourceTags, err := s.getResourceTags(accountID, resourceID)
		if err != nil {
			slog.Warn("DescribeTags failed to get tags", "resourceId", resourceID, "err", err)
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

	slog.Info("DescribeTags completed", "count", len(tags))

	return &ec2.DescribeTagsOutput{
		Tags: tags,
	}, nil
}

// DeleteTags removes tags from the specified resources
func (s *TagsServiceImpl) DeleteTags(input *ec2.DeleteTagsInput, accountID string) (*ec2.DeleteTagsOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	if len(input.Resources) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	slog.Info("DeleteTags request", "resources", len(input.Resources), "tags", len(input.Tags))

	for _, resourceID := range input.Resources {
		if resourceID == nil {
			continue
		}

		// Get existing tags
		existingTags, err := s.getResourceTags(accountID, *resourceID)
		if err != nil {
			slog.Error("DeleteTags failed to get existing tags", "resourceId", *resourceID, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}

		if len(input.Tags) == 0 {
			// Delete all tags if no specific tags provided
			existingTags = make(map[string]string)
		} else {
			// Delete specified tags — per AWS API, when Value is specified
			// the tag is only deleted if the stored value matches
			for _, tag := range input.Tags {
				if tag.Key == nil {
					continue
				}
				if tag.Value == nil {
					// No value specified: delete unconditionally
					delete(existingTags, *tag.Key)
				} else {
					// Value specified: only delete if current value matches
					if current, exists := existingTags[*tag.Key]; exists && current == *tag.Value {
						delete(existingTags, *tag.Key)
					}
				}
			}
		}

		// Save updated tags
		if err := s.putResourceTags(accountID, *resourceID, existingTags); err != nil {
			slog.Error("DeleteTags failed to save tags", "resourceId", *resourceID, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}

		slog.Info("DeleteTags applied", "resourceId", *resourceID, "remainingTags", len(existingTags))
	}

	return &ec2.DeleteTagsOutput{}, nil
}
