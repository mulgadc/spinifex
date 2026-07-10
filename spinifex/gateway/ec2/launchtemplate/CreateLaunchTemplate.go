package gateway_ec2_launchtemplate

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_launchtemplate "github.com/mulgadc/spinifex/spinifex/handlers/ec2/launchtemplate"
	"github.com/nats-io/nats.go"
)

// ValidateCreateLaunchTemplateInput checks the parameters required to create a
// template. Name format and DryRun semantics are owned by the daemon.
func ValidateCreateLaunchTemplateInput(input *ec2.CreateLaunchTemplateInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.LaunchTemplateData == nil {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if aws.StringValue(input.LaunchTemplateName) == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// CreateLaunchTemplate handles the EC2 CreateLaunchTemplate API call.
func CreateLaunchTemplate(ctx context.Context, input *ec2.CreateLaunchTemplateInput, natsConn *nats.Conn, accountID string) (ec2.CreateLaunchTemplateOutput, error) {
	var output ec2.CreateLaunchTemplateOutput

	if err := ValidateCreateLaunchTemplateInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_launchtemplate.NewNATSLaunchTemplateService(natsConn)
	result, err := svc.CreateLaunchTemplate(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
