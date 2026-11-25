// Package vpcpeering implements the EC2 VPC peering actions: it checks each
// request's shape and forwards it to the peering service over NATS.
package vpcpeering

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2vpcpeering "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpcpeering"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// requireConnectionID answers AWS's MissingParameter for an absent connection ID.
func requireConnectionID(id *string) error {
	if aws.StringValue(id) == "" {
		return awserrors.Errorf(awserrors.ErrorMissingParameter, "The request must contain the parameter vpcPeeringConnectionId")
	}
	return nil
}

// CreateVpcPeeringConnection forwards a peering request to the service scoped to accountID.
func CreateVpcPeeringConnection(ctx context.Context, input *ec2.CreateVpcPeeringConnectionInput, natsConn *nats.Conn, accountID string) (ec2.CreateVpcPeeringConnectionOutput, error) {
	var output ec2.CreateVpcPeeringConnectionOutput
	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	result, err := ec2vpcpeering.NewNATSVpcPeeringService(natsConn).CreateVpcPeeringConnection(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
