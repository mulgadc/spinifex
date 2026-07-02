package handlers_ec2_tags

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAccountID = "111111111111"

// setupTestTagsService creates a tags service with in-memory storage for testing
func setupTestTagsService(t *testing.T) (*TagsServiceImpl, *objectstore.MemoryObjectStore) {
	store := objectstore.NewMemoryObjectStore()
	cfg := &config.Config{
		Predastore: config.PredastoreConfig{
			Bucket: "test-bucket",
		},
	}

	svc := NewTagsServiceImplWithStore(cfg, store)
	return svc, store
}

// TestCreateTags tests adding tags to resources
func TestCreateTags(t *testing.T) {
	svc, _ := setupTestTagsService(t)

	// Create tags for an instance
	result, err := svc.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("i-test123")},
		Tags: []*ec2.Tag{
			{Key: aws.String("Name"), Value: aws.String("test-instance")},
			{Key: aws.String("Environment"), Value: aws.String("test")},
		},
	}, testAccountID)

	require.NoError(t, err)
	require.NotNil(t, result)

	// Verify tags were created
	describeResult, err := svc.DescribeTags(&ec2.DescribeTagsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("resource-id"), Values: []*string{aws.String("i-test123")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, describeResult.Tags, 2)
}

// TestCreateTags_MultipleResources tests adding tags to multiple resources
func TestCreateTags_MultipleResources(t *testing.T) {
	svc, _ := setupTestTagsService(t)

	// Create tags for multiple resources
	result, err := svc.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{
			aws.String("i-test1"),
			aws.String("i-test2"),
			aws.String("vol-test1"),
		},
		Tags: []*ec2.Tag{
			{Key: aws.String("Project"), Value: aws.String("test-project")},
		},
	}, testAccountID)

	require.NoError(t, err)
	require.NotNil(t, result)

	// Verify tags were created for all resources
	describeResult, err := svc.DescribeTags(&ec2.DescribeTagsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, describeResult.Tags, 3)
}

// TestCreateTags_UpdateExisting tests updating existing tags
func TestCreateTags_UpdateExisting(t *testing.T) {
	svc, _ := setupTestTagsService(t)

	// Create initial tag
	_, err := svc.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("i-test123")},
		Tags: []*ec2.Tag{
			{Key: aws.String("Name"), Value: aws.String("original")},
		},
	}, testAccountID)
	require.NoError(t, err)

	// Update the tag
	_, err = svc.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("i-test123")},
		Tags: []*ec2.Tag{
			{Key: aws.String("Name"), Value: aws.String("updated")},
		},
	}, testAccountID)
	require.NoError(t, err)

	// Verify tag was updated
	describeResult, err := svc.DescribeTags(&ec2.DescribeTagsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("resource-id"), Values: []*string{aws.String("i-test123")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, describeResult.Tags, 1)
	assert.Equal(t, "updated", *describeResult.Tags[0].Value)
}

// TestCreateTags_MissingResources tests creating tags without resources
func TestCreateTags_MissingResources(t *testing.T) {
	svc, _ := setupTestTagsService(t)

	_, err := svc.CreateTags(&ec2.CreateTagsInput{
		Tags: []*ec2.Tag{
			{Key: aws.String("Name"), Value: aws.String("test")},
		},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorMissingParameter)
}

// TestCreateTags_MissingTags tests creating tags without tags
func TestCreateTags_MissingTags(t *testing.T) {
	svc, _ := setupTestTagsService(t)

	_, err := svc.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("i-test123")},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorMissingParameter)
}

// TestDescribeTags tests listing all tags
func TestDescribeTags(t *testing.T) {
	svc, _ := setupTestTagsService(t)

	// Create tags for different resource types
	_, err := svc.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("i-test1")},
		Tags:      []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("instance1")}},
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("vol-test1")},
		Tags:      []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("volume1")}},
	}, testAccountID)
	require.NoError(t, err)

	// Describe all tags
	result, err := svc.DescribeTags(&ec2.DescribeTagsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, result.Tags, 2)
}

// TestDescribeTags_FilterByResourceID tests filtering by resource ID
func TestDescribeTags_FilterByResourceID(t *testing.T) {
	svc, _ := setupTestTagsService(t)

	// Create tags for different resources
	_, err := svc.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("i-test1")},
		Tags:      []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("instance1")}},
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("i-test2")},
		Tags:      []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("instance2")}},
	}, testAccountID)
	require.NoError(t, err)

	// Filter by resource ID
	result, err := svc.DescribeTags(&ec2.DescribeTagsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("resource-id"), Values: []*string{aws.String("i-test1")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, result.Tags, 1)
	assert.Equal(t, "i-test1", *result.Tags[0].ResourceId)
}

// TestDescribeTags_FilterByResourceType tests filtering by resource type
func TestDescribeTags_FilterByResourceType(t *testing.T) {
	svc, _ := setupTestTagsService(t)

	// Create tags for different resource types
	_, err := svc.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("i-test1")},
		Tags:      []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("instance1")}},
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("vol-test1")},
		Tags:      []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("volume1")}},
	}, testAccountID)
	require.NoError(t, err)

	// Filter by resource type
	result, err := svc.DescribeTags(&ec2.DescribeTagsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("resource-type"), Values: []*string{aws.String("instance")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, result.Tags, 1)
	assert.Equal(t, "instance", *result.Tags[0].ResourceType)
}

// TestDescribeTags_FilterByKey tests filtering by tag key
func TestDescribeTags_FilterByKey(t *testing.T) {
	svc, _ := setupTestTagsService(t)

	// Create tags with different keys
	_, err := svc.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("i-test1")},
		Tags: []*ec2.Tag{
			{Key: aws.String("Name"), Value: aws.String("instance1")},
			{Key: aws.String("Environment"), Value: aws.String("test")},
		},
	}, testAccountID)
	require.NoError(t, err)

	// Filter by key
	result, err := svc.DescribeTags(&ec2.DescribeTagsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("key"), Values: []*string{aws.String("Name")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, result.Tags, 1)
	assert.Equal(t, "Name", *result.Tags[0].Key)
}

// TestDescribeTags_FilterByValue tests filtering by tag value
func TestDescribeTags_FilterByValue(t *testing.T) {
	svc, _ := setupTestTagsService(t)

	// Create tags with different values
	_, err := svc.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("i-test1")},
		Tags: []*ec2.Tag{
			{Key: aws.String("Environment"), Value: aws.String("production")},
		},
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("i-test2")},
		Tags: []*ec2.Tag{
			{Key: aws.String("Environment"), Value: aws.String("test")},
		},
	}, testAccountID)
	require.NoError(t, err)

	// Filter by value
	result, err := svc.DescribeTags(&ec2.DescribeTagsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("value"), Values: []*string{aws.String("production")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, result.Tags, 1)
	assert.Equal(t, "production", *result.Tags[0].Value)
}

// TestDescribeTags_Empty tests listing tags when none exist
func TestDescribeTags_Empty(t *testing.T) {
	svc, _ := setupTestTagsService(t)

	result, err := svc.DescribeTags(&ec2.DescribeTagsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, result.Tags)
}

// TestDescribeTags_InvalidFilterName tests that unrecognized filter names return an error
func TestDescribeTags_InvalidFilterName(t *testing.T) {
	svc, _ := setupTestTagsService(t)

	_, err := svc.DescribeTags(&ec2.DescribeTagsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("resouce-id"), Values: []*string{aws.String("i-test1")}},
		},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
}

// TestDeleteTags tests deleting specific tags
func TestDeleteTags(t *testing.T) {
	svc, _ := setupTestTagsService(t)

	// Create tags
	_, err := svc.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("i-test123")},
		Tags: []*ec2.Tag{
			{Key: aws.String("Name"), Value: aws.String("test")},
			{Key: aws.String("Environment"), Value: aws.String("test")},
		},
	}, testAccountID)
	require.NoError(t, err)

	// Delete one tag
	_, err = svc.DeleteTags(&ec2.DeleteTagsInput{
		Resources: []*string{aws.String("i-test123")},
		Tags: []*ec2.Tag{
			{Key: aws.String("Name")},
		},
	}, testAccountID)
	require.NoError(t, err)

	// Verify only one tag remains
	result, err := svc.DescribeTags(&ec2.DescribeTagsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("resource-id"), Values: []*string{aws.String("i-test123")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, result.Tags, 1)
	assert.Equal(t, "Environment", *result.Tags[0].Key)
}

// TestDeleteTags_AllTags tests deleting all tags from a resource
func TestDeleteTags_AllTags(t *testing.T) {
	svc, _ := setupTestTagsService(t)

	// Create tags
	_, err := svc.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("i-test123")},
		Tags: []*ec2.Tag{
			{Key: aws.String("Name"), Value: aws.String("test")},
			{Key: aws.String("Environment"), Value: aws.String("test")},
		},
	}, testAccountID)
	require.NoError(t, err)

	// Delete all tags (no tags specified)
	_, err = svc.DeleteTags(&ec2.DeleteTagsInput{
		Resources: []*string{aws.String("i-test123")},
	}, testAccountID)
	require.NoError(t, err)

	// Verify no tags remain
	result, err := svc.DescribeTags(&ec2.DescribeTagsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("resource-id"), Values: []*string{aws.String("i-test123")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, result.Tags)
}

// TestDeleteTags_ValueConditional tests that DeleteTags only deletes when value matches
func TestDeleteTags_ValueConditional(t *testing.T) {
	svc, _ := setupTestTagsService(t)

	// Create tags
	_, err := svc.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("i-test123")},
		Tags: []*ec2.Tag{
			{Key: aws.String("Environment"), Value: aws.String("production")},
			{Key: aws.String("Team"), Value: aws.String("backend")},
		},
	}, testAccountID)
	require.NoError(t, err)

	// Try to delete Environment=staging (wrong value) — should NOT delete
	_, err = svc.DeleteTags(&ec2.DeleteTagsInput{
		Resources: []*string{aws.String("i-test123")},
		Tags: []*ec2.Tag{
			{Key: aws.String("Environment"), Value: aws.String("staging")},
		},
	}, testAccountID)
	require.NoError(t, err)

	// Verify both tags still exist
	result, err := svc.DescribeTags(&ec2.DescribeTagsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("resource-id"), Values: []*string{aws.String("i-test123")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, result.Tags, 2)

	// Delete Environment=production (correct value) — should delete
	_, err = svc.DeleteTags(&ec2.DeleteTagsInput{
		Resources: []*string{aws.String("i-test123")},
		Tags: []*ec2.Tag{
			{Key: aws.String("Environment"), Value: aws.String("production")},
		},
	}, testAccountID)
	require.NoError(t, err)

	// Verify only Team remains
	result, err = svc.DescribeTags(&ec2.DescribeTagsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("resource-id"), Values: []*string{aws.String("i-test123")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, result.Tags, 1)
	assert.Equal(t, "Team", *result.Tags[0].Key)
}

// TestDeleteTags_MissingResources tests deleting tags without resources
func TestDeleteTags_MissingResources(t *testing.T) {
	svc, _ := setupTestTagsService(t)

	_, err := svc.DeleteTags(&ec2.DeleteTagsInput{
		Tags: []*ec2.Tag{
			{Key: aws.String("Name")},
		},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorMissingParameter)
}

// TestGetResourceType tests the resource type detection helper
func TestGetResourceType(t *testing.T) {
	tests := []struct {
		resourceID   string
		expectedType string
	}{
		{"i-abc123", "instance"},
		{"vol-abc123", "volume"},
		{"ami-abc123", "image"},
		{"snap-abc123", "snapshot"},
		{"vpc-abc123", "vpc"},
		{"subnet-abc123", "subnet"},
		{"sg-abc123", "security-group"},
		{"rtb-abc123", "route-table"},
		{"igw-abc123", "internet-gateway"},
		{"eigw-abc123", "egress-only-internet-gateway"},
		{"eni-abc123", "network-interface"},
		{"eipalloc-abc123", "elastic-ip"},
		{"nat-abc123", "natgateway"},
		{"key-abc123", "key-pair"},
		{"pg-abc123", "placement-group"},
		{"unknown-abc123", "unknown"},
	}

	for _, tc := range tests {
		t.Run(tc.resourceID, func(t *testing.T) {
			assert.Equal(t, tc.expectedType, getResourceType(tc.resourceID))
		})
	}
}

// TestMemoryObjectStore tests the in-memory object store
func TestMemoryObjectStore(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()

	// Test that GetObject returns NoSuchKeyError for missing objects
	_, err := store.GetObject(&s3.GetObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String("nonexistent"),
	})
	require.Error(t, err)
	assert.True(t, objectstore.IsNoSuchKeyError(err))
}

// TestAccountIsolation_CreateAndDescribe tests that tags from one account are not visible to another
func TestAccountIsolation_CreateAndDescribe(t *testing.T) {
	svc, _ := setupTestTagsService(t)
	accountA := "111111111111"
	accountB := "222222222222"

	// Account A creates tags
	_, err := svc.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("i-aaa111")},
		Tags:      []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("account-a-instance")}},
	}, accountA)
	require.NoError(t, err)

	// Account B creates tags on a different resource
	_, err = svc.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("i-bbb222")},
		Tags:      []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("account-b-instance")}},
	}, accountB)
	require.NoError(t, err)

	// Account A should only see its own tags
	resultA, err := svc.DescribeTags(&ec2.DescribeTagsInput{}, accountA)
	require.NoError(t, err)
	assert.Len(t, resultA.Tags, 1)
	assert.Equal(t, "i-aaa111", *resultA.Tags[0].ResourceId)
	assert.Equal(t, "account-a-instance", *resultA.Tags[0].Value)

	// Account B should only see its own tags
	resultB, err := svc.DescribeTags(&ec2.DescribeTagsInput{}, accountB)
	require.NoError(t, err)
	assert.Len(t, resultB.Tags, 1)
	assert.Equal(t, "i-bbb222", *resultB.Tags[0].ResourceId)
	assert.Equal(t, "account-b-instance", *resultB.Tags[0].Value)
}

// TestAccountIsolation_SameResourceID tests that two accounts can tag the same resource ID independently
func TestAccountIsolation_SameResourceID(t *testing.T) {
	svc, _ := setupTestTagsService(t)
	accountA := "111111111111"
	accountB := "222222222222"

	// Both accounts tag the same resource ID
	_, err := svc.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("i-shared")},
		Tags:      []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("from-account-a")}},
	}, accountA)
	require.NoError(t, err)

	_, err = svc.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("i-shared")},
		Tags:      []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("from-account-b")}},
	}, accountB)
	require.NoError(t, err)

	// Each account sees its own tag value
	resultA, err := svc.DescribeTags(&ec2.DescribeTagsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("resource-id"), Values: []*string{aws.String("i-shared")}},
		},
	}, accountA)
	require.NoError(t, err)
	assert.Len(t, resultA.Tags, 1)
	assert.Equal(t, "from-account-a", *resultA.Tags[0].Value)

	resultB, err := svc.DescribeTags(&ec2.DescribeTagsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("resource-id"), Values: []*string{aws.String("i-shared")}},
		},
	}, accountB)
	require.NoError(t, err)
	assert.Len(t, resultB.Tags, 1)
	assert.Equal(t, "from-account-b", *resultB.Tags[0].Value)
}

// TestAccountIsolation_DeleteDoesNotAffectOtherAccount tests that deleting tags in one account doesn't affect another
func TestAccountIsolation_DeleteDoesNotAffectOtherAccount(t *testing.T) {
	svc, _ := setupTestTagsService(t)
	accountA := "111111111111"
	accountB := "222222222222"

	// Both accounts create tags on same resource ID
	_, err := svc.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("i-shared")},
		Tags:      []*ec2.Tag{{Key: aws.String("Env"), Value: aws.String("prod")}},
	}, accountA)
	require.NoError(t, err)

	_, err = svc.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("i-shared")},
		Tags:      []*ec2.Tag{{Key: aws.String("Env"), Value: aws.String("staging")}},
	}, accountB)
	require.NoError(t, err)

	// Account A deletes its tag
	_, err = svc.DeleteTags(&ec2.DeleteTagsInput{
		Resources: []*string{aws.String("i-shared")},
		Tags:      []*ec2.Tag{{Key: aws.String("Env")}},
	}, accountA)
	require.NoError(t, err)

	// Account A should have no tags
	resultA, err := svc.DescribeTags(&ec2.DescribeTagsInput{}, accountA)
	require.NoError(t, err)
	assert.Empty(t, resultA.Tags)

	// Account B's tag should still exist
	resultB, err := svc.DescribeTags(&ec2.DescribeTagsInput{}, accountB)
	require.NoError(t, err)
	assert.Len(t, resultB.Tags, 1)
	assert.Equal(t, "staging", *resultB.Tags[0].Value)
}
