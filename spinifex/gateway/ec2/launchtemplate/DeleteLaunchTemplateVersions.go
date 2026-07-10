package gateway_ec2_launchtemplate

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_launchtemplate "github.com/mulgadc/spinifex/spinifex/handlers/ec2/launchtemplate"
	"github.com/nats-io/nats.go"
)

// ValidateDeleteLaunchTemplateVersionsInput requires exactly one of id/name and
// at least one version to delete. Per-version outcomes (default-version refusal,
// missing versions) are reported by the daemon in the response.
func ValidateDeleteLaunchTemplateVersionsInput(input *ec2.DeleteLaunchTemplateVersionsInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if err := requireTemplateIdentity(input.LaunchTemplateId, input.LaunchTemplateName); err != nil {
		return err
	}
	if len(input.Versions) == 0 {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// DeleteLaunchTemplateVersions handles the EC2 DeleteLaunchTemplateVersions API call.
func DeleteLaunchTemplateVersions(ctx context.Context, input *ec2.DeleteLaunchTemplateVersionsInput, natsConn *nats.Conn, accountID string) (ec2.DeleteLaunchTemplateVersionsOutput, error) {
	var output ec2.DeleteLaunchTemplateVersionsOutput

	if err := ValidateDeleteLaunchTemplateVersionsInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_launchtemplate.NewNATSLaunchTemplateService(natsConn)
	result, err := svc.DeleteLaunchTemplateVersions(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
