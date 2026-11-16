package instance

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
)

// resettableInstanceAttributes are the only attributes AWS lets
// ResetInstanceAttribute name.
var resettableInstanceAttributes = map[string]bool{
	ec2.InstanceAttributeNameKernel:          true,
	ec2.InstanceAttributeNameRamdisk:         true,
	ec2.InstanceAttributeNameSourceDestCheck: true,
}

// ValidateResetInstanceAttributeInput checks the attribute before the
// instance ID, the order AWS checks them in.
func ValidateResetInstanceAttributeInput(input *ec2.ResetInstanceAttributeInput) error {
	if input == nil || input.Attribute == nil || *input.Attribute == "" {
		return awserrors.Errorf(awserrors.ErrorMissingParameter, "The request must contain the parameter attribute")
	}
	if !resettableInstanceAttributes[*input.Attribute] {
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "Value (%s) for parameter attribute is invalid. Unknown attribute.", *input.Attribute)
	}
	if input.InstanceId == nil || *input.InstanceId == "" {
		return awserrors.Errorf(awserrors.ErrorMissingParameter, "The request must contain the parameter instanceId")
	}
	if !instanceIDPattern.MatchString(*input.InstanceId) {
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "Value (%s) for parameter instanceId is invalid. Expected: 'i-...'.", *input.InstanceId)
	}
	return nil
}

// ResetInstanceAttribute validates the request and asks a daemon to confirm
// the instance exists in a state that allows the reset.
func ResetInstanceAttribute(ctx context.Context, input *ec2.ResetInstanceAttributeInput, natsConn *nats.Conn, accountID string) (*ec2.ResetInstanceAttributeOutput, error) {
	if err := ValidateResetInstanceAttributeInput(input); err != nil {
		return nil, err
	}
	return natsmsg.NATSRequest[ec2.ResetInstanceAttributeOutput](ctx, natsConn, "ec2.ResetInstanceAttribute", input, 30*time.Second, accountID)
}
