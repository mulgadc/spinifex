package gateway_ec2_launchtemplate

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_launchtemplate "github.com/mulgadc/spinifex/spinifex/handlers/ec2/launchtemplate"
	"github.com/nats-io/nats.go"
)

// ValidateModifyLaunchTemplateInput requires exactly one of id/name. DefaultVersion
// resolution (the only mutable field) is owned by the daemon.
func ValidateModifyLaunchTemplateInput(input *ec2.ModifyLaunchTemplateInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return requireTemplateIdentity(input.LaunchTemplateId, input.LaunchTemplateName)
}

// ModifyLaunchTemplate handles the EC2 ModifyLaunchTemplate API call.
func ModifyLaunchTemplate(ctx context.Context, input *ec2.ModifyLaunchTemplateInput, natsConn *nats.Conn, accountID string) (ec2.ModifyLaunchTemplateOutput, error) {
	var output ec2.ModifyLaunchTemplateOutput

	if err := ValidateModifyLaunchTemplateInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_launchtemplate.NewNATSLaunchTemplateService(natsConn)
	result, err := svc.ModifyLaunchTemplate(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
