package awsapi

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// RemoveTags handles the ELBv2 RemoveTags API call. It removes the given tag
// keys from each ELBv2 resource (load balancer, target group, listener,
// listener rule) named in ResourceArns. Removal is idempotent.
func RemoveTags(ctx context.Context, input *elbv2.RemoveTagsInput, natsConn *nats.Conn, accountID string) (elbv2.RemoveTagsOutput, error) {
	var output elbv2.RemoveTagsOutput

	if input == nil || len(input.ResourceArns) == 0 || len(input.TagKeys) == 0 {
		return output, errors.New(awserrors.ErrorMissingParameter)
	}

	svc := elbv2domain.NewNATSELBv2Service(natsConn)
	result, err := svc.RemoveTags(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
