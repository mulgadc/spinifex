package gateway_ec2_launchtemplate

import (
	"context"

	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_ec2_launchtemplate "github.com/mulgadc/spinifex/spinifex/handlers/ec2/launchtemplate"
	"github.com/nats-io/nats.go"
)

// DescribeLaunchTemplates handles the EC2 DescribeLaunchTemplates API call. No
// identity is required (describe-all is valid); id/name/filter matching is owned
// by the daemon.
func DescribeLaunchTemplates(ctx context.Context, input *ec2.DescribeLaunchTemplatesInput, natsConn *nats.Conn, accountID string) (ec2.DescribeLaunchTemplatesOutput, error) {
	var output ec2.DescribeLaunchTemplatesOutput

	svc := handlers_ec2_launchtemplate.NewNATSLaunchTemplateService(natsConn)
	result, err := svc.DescribeLaunchTemplates(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
