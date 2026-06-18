package handlers_imds

import (
	"encoding/base64"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	gateway_ec2_instance "github.com/mulgadc/spinifex/spinifex/gateway/ec2/instance"
	"github.com/nats-io/nats.go"
)

// natsInstanceLookup resolves instance-only metadata fields via DescribeInstances fan-out.
type natsInstanceLookup struct {
	nc            *nats.Conn
	expectedNodes int
}

var _ instanceLookup = (*natsInstanceLookup)(nil)

func (l *natsInstanceLookup) describe(accountID, instanceID string) (*instanceFacts, error) {
	out, err := gateway_ec2_instance.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	}, l.nc, l.expectedNodes, accountID)
	if err != nil {
		return nil, fmt.Errorf("describe instance %s: %w", instanceID, err)
	}

	inst := firstInstance(out)
	if inst == nil {
		return nil, nil // terminating or not yet visible; treat as miss
	}

	facts := &instanceFacts{
		instanceType: aws.StringValue(inst.InstanceType),
		imageID:      aws.StringValue(inst.ImageId),
		keyName:      aws.StringValue(inst.KeyName),
		architecture: aws.StringValue(inst.Architecture),
		pendingTime:  aws.TimeValue(inst.LaunchTime),
	}
	if inst.IamInstanceProfile != nil {
		facts.iamInstanceProfileArn = aws.StringValue(inst.IamInstanceProfile.Arn)
	}

	facts.userData = l.userData(accountID, instanceID)
	return facts, nil
}

// userData fetches and decodes the instance's base64 user-data, returning nil on miss or error.
func (l *natsInstanceLookup) userData(accountID, instanceID string) []byte {
	attr, err := gateway_ec2_instance.DescribeInstanceAttribute(&ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String(instanceID),
		Attribute:  aws.String("userData"),
	}, l.nc, l.expectedNodes, accountID)
	if err != nil || attr == nil || attr.UserData == nil || attr.UserData.Value == nil {
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(aws.StringValue(attr.UserData.Value))
	if err != nil {
		slog.Warn("IMDS: failed to decode instance user-data", "instance_id", instanceID, "err", err)
		return nil
	}
	return decoded
}

// firstInstance returns the first instance in a DescribeInstances response, or
// nil when the response carries none.
func firstInstance(out *ec2.DescribeInstancesOutput) *ec2.Instance {
	if out == nil {
		return nil
	}
	for _, res := range out.Reservations {
		if res == nil {
			continue
		}
		for _, inst := range res.Instances {
			if inst != nil {
				return inst
			}
		}
	}
	return nil
}
