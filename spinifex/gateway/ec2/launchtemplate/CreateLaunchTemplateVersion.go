package gateway_ec2_launchtemplate

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_launchtemplate "github.com/mulgadc/spinifex/spinifex/handlers/ec2/launchtemplate"
	"github.com/nats-io/nats.go"
)

// ValidateCreateLaunchTemplateVersionInput checks the version data is present
// and that exactly one of id/name addresses the parent template. SourceVersion
// resolution is owned by the daemon.
func ValidateCreateLaunchTemplateVersionInput(input *ec2.CreateLaunchTemplateVersionInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.LaunchTemplateData == nil {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return requireTemplateIdentity(input.LaunchTemplateId, input.LaunchTemplateName)
}

// CreateLaunchTemplateVersion handles the EC2 CreateLaunchTemplateVersion API call.
func CreateLaunchTemplateVersion(ctx context.Context, input *ec2.CreateLaunchTemplateVersionInput, natsConn *nats.Conn, accountID string) (ec2.CreateLaunchTemplateVersionOutput, error) {
	var output ec2.CreateLaunchTemplateVersionOutput

	if err := ValidateCreateLaunchTemplateVersionInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_launchtemplate.NewNATSLaunchTemplateService(natsConn)
	result, err := svc.CreateLaunchTemplateVersion(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
