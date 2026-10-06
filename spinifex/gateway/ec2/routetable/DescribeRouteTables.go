package gateway_ec2_routetable

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2routetable "github.com/mulgadc/spinifex/spinifex/domains/ec2/routetable"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

func DescribeRouteTables(ctx context.Context, input *ec2.DescribeRouteTablesInput, natsConn *nats.Conn, accountID string) (ec2.DescribeRouteTablesOutput, error) {
	var output ec2.DescribeRouteTablesOutput
	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	svc := ec2routetable.NewNATSRouteTableService(natsConn)
	result, err := svc.DescribeRouteTables(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
