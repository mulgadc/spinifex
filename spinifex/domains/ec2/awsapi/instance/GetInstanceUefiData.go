package instance

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// GetInstanceUefiData answers for an instance the account can see. It returns
// the instance ID without UefiData: no API exposes an instance's variable store.
func GetInstanceUefiData(ctx context.Context, input *ec2.GetInstanceUefiDataInput, natsConn *nats.Conn, expectedNodes int, nodeIDs []string, accountID string) (*ec2.GetInstanceUefiDataOutput, error) {
	if input == nil || input.InstanceId == nil || *input.InstanceId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if err := validateInstanceIDFormat([]*string{input.InstanceId}); err != nil {
		return nil, err
	}
	describe := &ec2.DescribeInstancesInput{InstanceIds: []*string{input.InstanceId}}
	if _, err := DescribeInstancesChecked(ctx, describe, natsConn, expectedNodes, nodeIDs, accountID); err != nil {
		return nil, err
	}
	return &ec2.GetInstanceUefiDataOutput{InstanceId: input.InstanceId}, nil
}
