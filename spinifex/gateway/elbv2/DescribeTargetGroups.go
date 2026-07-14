package gateway_elbv2

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

// DescribeTargetGroups handles the ELBv2 DescribeTargetGroups API call.
func DescribeTargetGroups(ctx context.Context, input *elbv2.DescribeTargetGroupsInput, natsConn *nats.Conn, accountID string) (elbv2.DescribeTargetGroupsOutput, error) {
	var output elbv2.DescribeTargetGroupsOutput

	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	result, err := svc.DescribeTargetGroups(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
