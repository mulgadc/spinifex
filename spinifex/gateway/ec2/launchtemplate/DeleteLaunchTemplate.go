package gateway_ec2_launchtemplate

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_launchtemplate "github.com/mulgadc/spinifex/spinifex/handlers/ec2/launchtemplate"
	"github.com/nats-io/nats.go"
)

// ValidateDeleteLaunchTemplateInput requires exactly one of id/name.
func ValidateDeleteLaunchTemplateInput(input *ec2.DeleteLaunchTemplateInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return requireTemplateIdentity(input.LaunchTemplateId, input.LaunchTemplateName)
}

// DeleteLaunchTemplate handles the EC2 DeleteLaunchTemplate API call.
func DeleteLaunchTemplate(ctx context.Context, input *ec2.DeleteLaunchTemplateInput, natsConn *nats.Conn, accountID string) (ec2.DeleteLaunchTemplateOutput, error) {
	var output ec2.DeleteLaunchTemplateOutput

	if err := ValidateDeleteLaunchTemplateInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_launchtemplate.NewNATSLaunchTemplateService(natsConn)
	result, err := svc.DeleteLaunchTemplate(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
