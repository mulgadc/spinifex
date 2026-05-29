package gateway_elbv2

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

func ValidateDescribeTargetHealthInput(input *elbv2.DescribeTargetHealthInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.TargetGroupArn == nil || *input.TargetGroupArn == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// DescribeTargetHealth handles the ELBv2 DescribeTargetHealth API call.
func DescribeTargetHealth(input *elbv2.DescribeTargetHealthInput, natsConn *nats.Conn, accountID string) (elbv2.DescribeTargetHealthOutput, error) {
	var output elbv2.DescribeTargetHealthOutput

	if err := ValidateDescribeTargetHealthInput(input); err != nil {
		return output, err
	}

	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	result, err := svc.DescribeTargetHealth(input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
