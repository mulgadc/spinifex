// Package tags implements the EC2 tagging actions: the TagsService
// contract, its daemon-side JetStream KV implementation and its NATS client.
package tags

import (
	"context"

	"github.com/aws/aws-sdk-go/service/ec2"
)

// TagsService defines the interface for EC2 tag operations.
type TagsService interface {
	CreateTags(ctx context.Context, input *ec2.CreateTagsInput, accountID string) (*ec2.CreateTagsOutput, error)
	DescribeTags(ctx context.Context, input *ec2.DescribeTagsInput, accountID string) (*ec2.DescribeTagsOutput, error)
	DeleteTags(ctx context.Context, input *ec2.DeleteTagsInput, accountID string) (*ec2.DeleteTagsOutput, error)
}
