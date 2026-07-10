package gateway_ec2_launchtemplate

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_launchtemplate "github.com/mulgadc/spinifex/spinifex/handlers/ec2/launchtemplate"
	"github.com/nats-io/nats.go"
)

// ValidateDescribeLaunchTemplateVersionsInput requires exactly one of id/name.
// Version selection ($Default/$Latest/numeric, Min/Max range) is owned by the
// daemon.
func ValidateDescribeLaunchTemplateVersionsInput(input *ec2.DescribeLaunchTemplateVersionsInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return requireTemplateIdentity(input.LaunchTemplateId, input.LaunchTemplateName)
}

// DescribeLaunchTemplateVersions handles the EC2 DescribeLaunchTemplateVersions API call.
func DescribeLaunchTemplateVersions(ctx context.Context, input *ec2.DescribeLaunchTemplateVersionsInput, natsConn *nats.Conn, accountID string) (ec2.DescribeLaunchTemplateVersionsOutput, error) {
	var output ec2.DescribeLaunchTemplateVersionsOutput

	if err := ValidateDescribeLaunchTemplateVersionsInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_launchtemplate.NewNATSLaunchTemplateService(natsConn)
	result, err := svc.DescribeLaunchTemplateVersions(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
