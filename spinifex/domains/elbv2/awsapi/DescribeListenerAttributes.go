package awsapi

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// DescribeListenerAttributesInput is the ELBv2 DescribeListenerAttributes request, defined
// locally because aws-sdk-go v1's elbv2 package has no such shape.
type DescribeListenerAttributesInput struct {
	_           struct{} `type:"structure"`
	ListenerArn *string  `type:"string"`
}

// ListenerAttribute is one Key/Value listener attribute in the ELBv2 Query wire format.
type ListenerAttribute struct {
	_     struct{} `type:"structure"`
	Key   *string  `type:"string"`
	Value *string  `type:"string"`
}

// DescribeListenerAttributesOutput is the ELBv2 DescribeListenerAttributes response, encoded as
// an Attributes member list.
type DescribeListenerAttributesOutput struct {
	_          struct{}            `type:"structure"`
	Attributes []ListenerAttribute `locationName:"Attributes" locationNameList:"member" type:"list"`
}

// ValidateDescribeListenerAttributesInput rejects a nil input with InvalidParameterValue and a
// missing ListenerArn with MissingParameter.
func ValidateDescribeListenerAttributesInput(input *DescribeListenerAttributesInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.ListenerArn == nil || *input.ListenerArn == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// tcpIdleTimeoutDefault is the AWS default tcp.idle_timeout.seconds of a TCP listener.
const tcpIdleTimeoutDefault = "350"

// DescribeListenerAttributes looks the listener up and returns its protocol's
// defaults; listener attributes are not persisted, so no stored value overrides them.
func DescribeListenerAttributes(ctx context.Context, input *DescribeListenerAttributesInput, natsConn *nats.Conn, accountID string) (DescribeListenerAttributesOutput, error) {
	if err := ValidateDescribeListenerAttributesInput(input); err != nil {
		return DescribeListenerAttributesOutput{}, err
	}
	svc := elbv2domain.NewNATSELBv2Service(natsConn)
	result, err := svc.DescribeListeners(ctx, &elbv2.DescribeListenersInput{ListenerArns: []*string{input.ListenerArn}}, accountID)
	if err != nil {
		return DescribeListenerAttributesOutput{}, err
	}
	if len(result.Listeners) == 0 {
		return DescribeListenerAttributesOutput{}, awserrors.Errorf(awserrors.ErrorELBv2ListenerNotFound, "One or more listeners not found")
	}
	attrs := []ListenerAttribute{}
	if aws.StringValue(result.Listeners[0].Protocol) == elbv2.ProtocolEnumTcp {
		attrs = append(attrs, ListenerAttribute{Key: aws.String("tcp.idle_timeout.seconds"), Value: aws.String(tcpIdleTimeoutDefault)})
	}
	return DescribeListenerAttributesOutput{Attributes: attrs}, nil
}
