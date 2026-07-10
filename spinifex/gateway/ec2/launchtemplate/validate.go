package gateway_ec2_launchtemplate

import (
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// requireTemplateIdentity enforces the id-XOR-name rule shared by the actions
// that address an existing template. Both set is invalid, neither is a missing
// parameter. Error codes match the daemon-side resolver so behaviour is
// identical whether or not the request reaches the daemon.
func requireTemplateIdentity(id, name *string) error {
	idSet := aws.StringValue(id) != ""
	nameSet := aws.StringValue(name) != ""
	switch {
	case idSet && nameSet:
		return errors.New(awserrors.ErrorInvalidParameterValue)
	case !idSet && !nameSet:
		return errors.New(awserrors.ErrorMissingParameter)
	default:
		return nil
	}
}
