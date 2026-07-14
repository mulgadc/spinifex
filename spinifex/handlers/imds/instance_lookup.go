package handlers_imds

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	gateway_ec2_instance "github.com/mulgadc/spinifex/spinifex/gateway/ec2/instance"
	"github.com/nats-io/nats.go"
)

// imdsLookupRetries bounds transient-gather retries for IMDS instance lookups so
// a momentary NATS gather timeout under load does not starve the guest bootstrap.
const imdsLookupRetries = 3

// retryBackoff sleeps a short increasing delay between IMDS lookup attempts,
// returning false if ctx is cancelled first.
func retryBackoff(ctx context.Context, attempt int) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(time.Duration(attempt) * 200 * time.Millisecond):
		return true
	}
}

// retryGather retries fn on transient error up to imdsLookupRetries times with a
// short backoff, honouring ctx. Returns the last result and error.
func retryGather[T any](ctx context.Context, label, instanceID string, fn func() (*T, error)) (*T, error) {
	var out *T
	var err error
	for attempt := 1; attempt <= imdsLookupRetries; attempt++ {
		out, err = fn()
		if err == nil {
			return out, nil
		}
		if attempt == imdsLookupRetries || !retryBackoff(ctx, attempt) {
			return out, err
		}
		slog.WarnContext(ctx, "IMDS: "+label+" failed, retrying", "instance_id", instanceID, "attempt", attempt, "err", err)
	}
	return out, err
}

// natsInstanceLookup resolves instance-only metadata fields via DescribeInstances fan-out.
type natsInstanceLookup struct {
	nc            *nats.Conn
	expectedNodes int
}

var _ instanceLookup = (*natsInstanceLookup)(nil)

func (l *natsInstanceLookup) describe(ctx context.Context, accountID, instanceID string) (*instanceFacts, error) {
	out, err := retryGather(ctx, "describe instance", instanceID, func() (*ec2.DescribeInstancesOutput, error) {
		return gateway_ec2_instance.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: []*string{aws.String(instanceID)},
		}, l.nc, l.expectedNodes, accountID)
	})
	if err != nil {
		return nil, fmt.Errorf("describe instance %s: %w", instanceID, err)
	}

	res, inst := firstReservationInstance(out)
	if inst == nil {
		return nil, nil // terminating or not yet visible; treat as miss
	}

	facts := &instanceFacts{
		instanceType:   aws.StringValue(inst.InstanceType),
		imageID:        aws.StringValue(inst.ImageId),
		keyName:        aws.StringValue(inst.KeyName),
		architecture:   aws.StringValue(inst.Architecture),
		amiLaunchIndex: aws.Int64Value(inst.AmiLaunchIndex),
		reservationID:  aws.StringValue(res.ReservationId),
		lifecycleType:  aws.StringValue(inst.InstanceLifecycle),
		pendingTime:    aws.TimeValue(inst.LaunchTime),
	}
	if inst.IamInstanceProfile != nil {
		facts.iamInstanceProfileArn = aws.StringValue(inst.IamInstanceProfile.Arn)
	}

	facts.userData = l.userData(ctx, accountID, instanceID)
	return facts, nil
}

// userData fetches and decodes the instance's base64 user-data, returning nil on miss or error.
func (l *natsInstanceLookup) userData(ctx context.Context, accountID, instanceID string) []byte {
	attr, err := retryGather(ctx, "describe instance attribute", instanceID, func() (*ec2.DescribeInstanceAttributeOutput, error) {
		return gateway_ec2_instance.DescribeInstanceAttribute(ctx, &ec2.DescribeInstanceAttributeInput{
			InstanceId: aws.String(instanceID),
			Attribute:  aws.String("userData"),
		}, l.nc, l.expectedNodes, accountID)
	})
	if err != nil || attr == nil || attr.UserData == nil || attr.UserData.Value == nil {
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(aws.StringValue(attr.UserData.Value))
	if err != nil {
		slog.WarnContext(ctx, "IMDS: failed to decode instance user-data", "instance_id", instanceID, "err", err)
		return nil
	}
	return decoded
}

// firstReservationInstance returns the first instance in a DescribeInstances
// response along with its owning reservation, or (nil, nil) when none. The
// reservation is always non-nil when the instance is, since an instance is only
// returned from a non-nil reservation that contains it.
func firstReservationInstance(out *ec2.DescribeInstancesOutput) (*ec2.Reservation, *ec2.Instance) {
	if out == nil {
		return nil, nil
	}
	for _, res := range out.Reservations {
		if res == nil {
			continue
		}
		for _, inst := range res.Instances {
			if inst != nil {
				return res, inst
			}
		}
	}
	return nil, nil
}
