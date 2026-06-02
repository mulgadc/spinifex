package gateway_elbv2

import (
	"errors"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

type DescribeListenerAttributesInput struct {
	_           struct{} `type:"structure"`
	ListenerArn *string  `type:"string"`
}

type ListenerAttribute struct {
	_     struct{} `type:"structure"`
	Key   *string  `type:"string"`
	Value *string  `type:"string"`
}

type DescribeListenerAttributesOutput struct {
	_          struct{}            `type:"structure"`
	Attributes []ListenerAttribute `locationName:"Attributes" locationNameList:"member" type:"list"`
}

func ValidateDescribeListenerAttributesInput(input *DescribeListenerAttributesInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.ListenerArn == nil || *input.ListenerArn == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// DescribeListenerAttributes returns an empty attribute set. Listener-level
// attributes (TCP idle timeout, HTTP/2 toggle, etc.) are not persisted by
// spinifex; the provider observes defaults across refresh cycles.
func DescribeListenerAttributes(input *DescribeListenerAttributesInput, accountID string) (DescribeListenerAttributesOutput, error) {
	if err := ValidateDescribeListenerAttributesInput(input); err != nil {
		return DescribeListenerAttributesOutput{}, err
	}
	return DescribeListenerAttributesOutput{Attributes: []ListenerAttribute{}}, nil
}
