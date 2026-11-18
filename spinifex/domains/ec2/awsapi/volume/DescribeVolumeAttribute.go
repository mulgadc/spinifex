package volume

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// DescribeVolumeAttribute reads the volume through DescribeVolumes. I/O is never
// suspended on an impaired volume and no volume carries a product code, so
// autoEnableIO is false and productCodes is empty.
func DescribeVolumeAttribute(ctx context.Context, input *ec2.DescribeVolumeAttributeInput, natsConn *nats.Conn, accountID string) (ec2.DescribeVolumeAttributeOutput, error) {
	var output ec2.DescribeVolumeAttributeOutput
	if input == nil || aws.StringValue(input.VolumeId) == "" || aws.StringValue(input.Attribute) == "" {
		return output, errors.New(awserrors.ErrorMissingParameter)
	}
	describe := &ec2.DescribeVolumesInput{VolumeIds: []*string{input.VolumeId}}
	if err := ValidateDescribeVolumesInput(describe); err != nil {
		return output, err
	}
	attribute := aws.StringValue(input.Attribute)
	if attribute != ec2.VolumeAttributeNameAutoEnableIo && attribute != ec2.VolumeAttributeNameProductCodes {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	volumes, err := DescribeVolumes(ctx, describe, natsConn, accountID)
	if err != nil {
		return output, err
	}
	if len(volumes.Volumes) == 0 {
		return output, errors.New(awserrors.ErrorInvalidVolumeNotFound)
	}

	output.VolumeId = volumes.Volumes[0].VolumeId
	if attribute == ec2.VolumeAttributeNameAutoEnableIo {
		output.AutoEnableIO = &ec2.AttributeBooleanValue{Value: aws.Bool(false)}
	} else {
		output.ProductCodes = []*ec2.ProductCode{}
	}
	return output, nil
}
