package gateway_elbv2

import (
	"errors"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

type ModifyListenerAttributesInput struct {
	_           struct{}            `type:"structure"`
	ListenerArn *string             `type:"string"`
	Attributes  []ListenerAttribute `locationName:"Attributes" locationNameList:"member" type:"list"`
}

type ModifyListenerAttributesOutput struct {
	_          struct{}            `type:"structure"`
	Attributes []ListenerAttribute `locationName:"Attributes" locationNameList:"member" type:"list"`
}

func ValidateModifyListenerAttributesInput(input *ModifyListenerAttributesInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.ListenerArn == nil || *input.ListenerArn == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// ModifyListenerAttributes accepts the request and echoes the attributes back.
// Listener attributes are not persisted; this stub keeps clients (e.g.,
// terraform-provider-aws) from erroring on unimplemented actions.
func ModifyListenerAttributes(input *ModifyListenerAttributesInput, accountID string) (ModifyListenerAttributesOutput, error) {
	if err := ValidateModifyListenerAttributesInput(input); err != nil {
		return ModifyListenerAttributesOutput{}, err
	}
	return ModifyListenerAttributesOutput{Attributes: input.Attributes}, nil
}
