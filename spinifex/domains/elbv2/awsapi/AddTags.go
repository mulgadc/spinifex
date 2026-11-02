package awsapi

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// AddTags handles the ELBv2 AddTags API call. It adds or overwrites the given
// tags on each ELBv2 resource (load balancer, target group, listener, listener
// rule) named in ResourceArns.
func AddTags(ctx context.Context, input *elbv2.AddTagsInput, natsConn *nats.Conn, accountID string) (elbv2.AddTagsOutput, error) {
	var output elbv2.AddTagsOutput

	if input == nil || len(input.ResourceArns) == 0 || len(input.Tags) == 0 {
		return output, errors.New(awserrors.ErrorMissingParameter)
	}

	svc := elbv2domain.NewNATSELBv2Service(natsConn)
	result, err := svc.AddTags(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
