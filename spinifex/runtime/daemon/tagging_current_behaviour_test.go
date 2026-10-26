package daemon

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The TestCurrentBehaviour_Tagging_ tests pin what the daemon's EC2 tag
// handlers accept today. They describe current behaviour, not the target
// design, and a failure means the behaviour changed.

// A non-instance ID is never resolved to a resource before the central write.
// Another account's ID, a well-formed ID naming nothing, and an ID with no
// recognised prefix all succeed, and each is stored under the caller's account.
func TestCurrentBehaviour_Tagging_EC2CreateTagsStoresUnresolvedIDsInCallerNamespace(t *testing.T) {
	const caller = "999999999999"
	d := tagTestDaemon(t, "i-tag-cb-unrelated", nil)

	ids := []string{"sg-0a1b2c3d4e5f60718", "vol-00000000000000000", "not-a-real-id"}
	resources := make([]*string, 0, len(ids))
	for _, id := range ids {
		resources = append(resources, aws.String(id))
	}

	_, err := d.createTags(context.Background(), &ec2.CreateTagsInput{
		Resources: resources,
		Tags:      []*ec2.Tag{{Key: aws.String("env"), Value: aws.String("prod")}},
	}, caller)
	require.NoError(t, err)

	for _, id := range ids {
		assert.Equal(t, map[string]string{"env": "prod"}, centralTags(t, d, caller, id), id)
		assert.Empty(t, centralTags(t, d, testAccountID, id), id)
	}

	out, err := d.tagsService.DescribeTags(t.Context(), &ec2.DescribeTagsInput{
		Filters: []*ec2.Filter{{Name: aws.String("resource-id"), Values: []*string{aws.String("not-a-real-id")}}},
	}, caller)
	require.NoError(t, err)
	require.Len(t, out.Tags, 1)
	assert.Equal(t, "unknown", aws.StringValue(out.Tags[0].ResourceType))
}
