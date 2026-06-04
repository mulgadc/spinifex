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

// natsInstanceLookup resolves the instance-only metadata fields via the same
// account-scoped NATS fan-out the gateway's DescribeInstances path uses, since
// the instance record lives in each daemon's in-memory manager, not a central KV.
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
		// The ENI references an instance the daemons no longer report (mid
		// terminate, or not yet visible). Treated as a miss → 404.
		return nil, nil
	}

	facts := &instanceFacts{
		instanceType: aws.StringValue(inst.InstanceType),
		imageID:      aws.StringValue(inst.ImageId),
	}
	if inst.IamInstanceProfile != nil {
		facts.iamInstanceProfileArn = aws.StringValue(inst.IamInstanceProfile.Arn)
	}

	facts.userData = l.userData(accountID, instanceID)
	return facts, nil
}

// userData fetches the base64 user-data blob via DescribeInstanceAttribute and
// decodes it. A miss or decode failure yields nil — /latest/user-data then 404s,
// matching AWS for instances launched without user-data.
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
