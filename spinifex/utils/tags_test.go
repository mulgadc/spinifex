package utils

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
)

func TestExtractTags(t *testing.T) {
	specs := []*ec2.TagSpecification{
		{
			ResourceType: aws.String("instance"),
			Tags: []*ec2.Tag{
				{Key: aws.String("Name"), Value: aws.String("web-1")},
				{Key: nil, Value: aws.String("skipped-nil-key")},
				{Key: aws.String("skipped-nil-value"), Value: nil},
			},
		},
		{
			ResourceType: aws.String("volume"),
			Tags: []*ec2.Tag{
				{Key: aws.String("Env"), Value: aws.String("prod")},
			},
		},
	}

	got := ExtractTags(specs, "instance")
	assert.Equal(t, map[string]string{"Name": "web-1"}, got)

	// Non-matching resource type returns an empty (non-nil) map.
	empty := ExtractTags(specs, "snapshot")
	assert.NotNil(t, empty)
	assert.Empty(t, empty)
}

func TestMapToEC2Tags_Populated(t *testing.T) {
	got := MapToEC2Tags(map[string]string{
		"Name": "web-1",
		"Env":  "prod",
	})
	assert.Len(t, got, 2)
	asMap := make(map[string]string, len(got))
	for _, tag := range got {
		asMap[aws.StringValue(tag.Key)] = aws.StringValue(tag.Value)
	}
	assert.Equal(t, map[string]string{"Name": "web-1", "Env": "prod"}, asMap)
}

func TestMapToEC2Tags_Empty(t *testing.T) {
	assert.Nil(t, MapToEC2Tags(nil))
	assert.Nil(t, MapToEC2Tags(map[string]string{}))
}
