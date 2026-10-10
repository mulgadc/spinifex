// Package tags implements the EC2 tagging actions: it validates
// each request and forwards it to the tag service over NATS.
package tags

import (
	"context"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2tags "github.com/mulgadc/spinifex/spinifex/domains/ec2/tags"
	"github.com/nats-io/nats.go"
)

// DescribeTags handles the EC2 DescribeTags API call.
func DescribeTags(ctx context.Context, input *ec2.DescribeTagsInput, natsConn *nats.Conn, accountID string) (ec2.DescribeTagsOutput, error) {
	var output ec2.DescribeTagsOutput

	svc := ec2tags.NewNATSTagsService(natsConn)
	result, err := svc.DescribeTags(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
