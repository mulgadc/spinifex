package gateway_elbv2

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

func ValidateDescribeTargetGroupAttributesInput(input *elbv2.DescribeTargetGroupAttributesInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.TargetGroupArn == nil || *input.TargetGroupArn == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// DescribeTargetGroupAttributes handles the ELBv2 DescribeTargetGroupAttributes API call.
func DescribeTargetGroupAttributes(input *elbv2.DescribeTargetGroupAttributesInput, natsConn *nats.Conn, accountID string) (elbv2.DescribeTargetGroupAttributesOutput, error) {
	var output elbv2.DescribeTargetGroupAttributesOutput

	if err := ValidateDescribeTargetGroupAttributesInput(input); err != nil {
		return output, err
	}

	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	result, err := svc.DescribeTargetGroupAttributes(input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
