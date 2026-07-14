package gateway_ec2_instance

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDescribeInstances_SingleNode(t *testing.T) {
	_, nc := startTestNATSServer(t)

	reservation := &ec2.Reservation{
		ReservationId: aws.String("r-abc123"),
		Instances: []*ec2.Instance{
			{
				InstanceId:   aws.String("i-001"),
				InstanceType: aws.String("t3.micro"),
				State:        &ec2.InstanceState{Code: aws.Int64(16), Name: aws.String("running")},
			},
		},
	}

	_, err := nc.Subscribe("ec2.DescribeInstances", func(msg *nats.Msg) {
		data, _ := json.Marshal(&ec2.DescribeInstancesOutput{
			Reservations: []*ec2.Reservation{reservation},
		})
		msg.Respond(data)
	})
	require.NoError(t, err)

	input := &ec2.DescribeInstancesInput{}
	output, err := DescribeInstances(context.Background(), input, nc, 1, "123456789012")

	require.NoError(t, err)
	require.NotNil(t, output)
	require.Len(t, output.Reservations, 1)
	assert.Equal(t, "r-abc123", *output.Reservations[0].ReservationId)
	assert.Equal(t, "i-001", *output.Reservations[0].Instances[0].InstanceId)
}

func TestDescribeInstances_MultipleNodes(t *testing.T) {
	_, nc := startTestNATSServer(t)

	// Two nodes each return different instances
	nc.Subscribe("ec2.DescribeInstances", func(msg *nats.Msg) {
		data, _ := json.Marshal(&ec2.DescribeInstancesOutput{
			Reservations: []*ec2.Reservation{
				{
					ReservationId: aws.String("r-node1"),
					Instances: []*ec2.Instance{
						{InstanceId: aws.String("i-node1-001")},
					},
				},
			},
		})
		msg.Respond(data)
	})

	nc2, err := nats.Connect(nc.ConnectedUrl())
	require.NoError(t, err)
	defer nc2.Close()

	nc2.Subscribe("ec2.DescribeInstances", func(msg *nats.Msg) {
		data, _ := json.Marshal(&ec2.DescribeInstancesOutput{
			Reservations: []*ec2.Reservation{
				{
					ReservationId: aws.String("r-node2"),
					Instances: []*ec2.Instance{
						{InstanceId: aws.String("i-node2-001")},
						{InstanceId: aws.String("i-node2-002")},
					},
				},
			},
		})
		msg.Respond(data)
	})

	// Wait for subscriptions to propagate
	nc.Flush()
	nc2.Flush()

	input := &ec2.DescribeInstancesInput{}
	output, err := DescribeInstances(context.Background(), input, nc, 2, "123456789012")

	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Len(t, output.Reservations, 2)
}

func TestDescribeInstances_NoSubscribers(t *testing.T) {
	_, nc := startTestNATSServer(t)

	input := &ec2.DescribeInstancesInput{}
	output, err := DescribeInstances(context.Background(), input, nc, 0, "123456789012")

	// No subscribers means timeout — function returns empty reservations, no error
	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Empty(t, output.Reservations)
}

func TestDescribeInstances_NodeReturnsError(t *testing.T) {
	_, nc := startTestNATSServer(t)

	nc.Subscribe("ec2.DescribeInstances", func(msg *nats.Msg) {
		errorPayload := utils.GenerateErrorPayload("InternalError")
		msg.Respond(errorPayload)
	})

	input := &ec2.DescribeInstancesInput{}
	output, err := DescribeInstances(context.Background(), input, nc, 1, "123456789012")

	// Error responses from nodes are logged but don't fail the overall call
	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Empty(t, output.Reservations)
}

func TestDescribeInstances_MixedResponses(t *testing.T) {
	_, nc := startTestNATSServer(t)

	// Node 1: returns valid data
	nc.Subscribe("ec2.DescribeInstances", func(msg *nats.Msg) {
		data, _ := json.Marshal(&ec2.DescribeInstancesOutput{
			Reservations: []*ec2.Reservation{
				{
					ReservationId: aws.String("r-good"),
					Instances:     []*ec2.Instance{{InstanceId: aws.String("i-good")}},
				},
			},
		})
		msg.Respond(data)
	})

	// Node 2: returns error
	nc2, err := nats.Connect(nc.ConnectedUrl())
	require.NoError(t, err)
	defer nc2.Close()

	nc2.Subscribe("ec2.DescribeInstances", func(msg *nats.Msg) {
		errorPayload := utils.GenerateErrorPayload("InternalError")
		msg.Respond(errorPayload)
	})

	nc.Flush()
	nc2.Flush()

	input := &ec2.DescribeInstancesInput{}
	output, err := DescribeInstances(context.Background(), input, nc, 2, "123456789012")

	require.NoError(t, err)
	require.NotNil(t, output)
	// Only the valid node's reservation should appear
	assert.Len(t, output.Reservations, 1)
	assert.Equal(t, "r-good", *output.Reservations[0].ReservationId)
}

func TestDescribeInstances_MalformedJSON(t *testing.T) {
	_, nc := startTestNATSServer(t)

	nc.Subscribe("ec2.DescribeInstances", func(msg *nats.Msg) {
		msg.Respond([]byte(`{invalid json`))
	})

	input := &ec2.DescribeInstancesInput{}
	output, err := DescribeInstances(context.Background(), input, nc, 1, "123456789012")

	// Malformed JSON from a node is skipped, not fatal
	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Empty(t, output.Reservations)
}

func TestDescribeInstances_EmptyReservations(t *testing.T) {
	_, nc := startTestNATSServer(t)

	nc.Subscribe("ec2.DescribeInstances", func(msg *nats.Msg) {
		data, _ := json.Marshal(&ec2.DescribeInstancesOutput{
			Reservations: nil,
		})
		msg.Respond(data)
	})

	input := &ec2.DescribeInstancesInput{}
	output, err := DescribeInstances(context.Background(), input, nc, 1, "123456789012")

	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Empty(t, output.Reservations)
}

func TestDescribeInstances_TimeoutCollection(t *testing.T) {
	_, nc := startTestNATSServer(t)

	// Node responds after a delay (but within timeout)
	nc.Subscribe("ec2.DescribeInstances", func(msg *nats.Msg) {
		time.Sleep(500 * time.Millisecond)
		data, _ := json.Marshal(&ec2.DescribeInstancesOutput{
			Reservations: []*ec2.Reservation{
				{ReservationId: aws.String("r-delayed")},
			},
		})
		msg.Respond(data)
	})

	input := &ec2.DescribeInstancesInput{}
	output, err := DescribeInstances(context.Background(), input, nc, 0, "123456789012") // 0 = timeout-based collection

	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Len(t, output.Reservations, 1)
}

func TestDescribeInstances_EarlyExitWithExpectedNodes(t *testing.T) {
	_, nc := startTestNATSServer(t)

	nc.Subscribe("ec2.DescribeInstances", func(msg *nats.Msg) {
		data, _ := json.Marshal(&ec2.DescribeInstancesOutput{
			Reservations: []*ec2.Reservation{
				{ReservationId: aws.String("r-fast")},
			},
		})
		msg.Respond(data)
	})

	input := &ec2.DescribeInstancesInput{}
	start := time.Now()
	output, err := DescribeInstances(context.Background(), input, nc, 1, "123456789012")
	duration := time.Since(start)

	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Len(t, output.Reservations, 1)
	// Should exit early, well before the 3-second timeout
	assert.Less(t, duration, 2*time.Second)
}

func TestDescribeInstances_ClosedConnection(t *testing.T) {
	_, nc := startTestNATSServer(t)

	closedNC, err := nats.Connect(nc.ConnectedUrl())
	require.NoError(t, err)
	closedNC.Close()

	input := &ec2.DescribeInstancesInput{}
	_, err = DescribeInstances(context.Background(), input, closedNC, 1, "123456789012")

	require.Error(t, err)
}

// describeOutputWithProfiles builds a DescribeInstancesOutput with one
// reservation whose instances each carry the supplied ARN (empty string =
// no profile attached).
func describeOutputWithProfiles(arns ...string) *ec2.DescribeInstancesOutput {
	out := &ec2.DescribeInstancesOutput{
		Reservations: []*ec2.Reservation{{ReservationId: aws.String("r-1")}},
	}
	for i, arn := range arns {
		inst := &ec2.Instance{InstanceId: aws.String("i-" + string(rune('a'+i)))}
		if arn != "" {
			inst.IamInstanceProfile = &ec2.IamInstanceProfile{Arn: aws.String(arn)}
		}
		out.Reservations[0].Instances = append(out.Reservations[0].Instances, inst)
	}
	return out
}

func TestEnrichInstanceProfileIDs_CachesPerARN(t *testing.T) {
	arn := "arn:aws:iam::123456789012:instance-profile/shared"
	var calls int
	svc := &fakeIAMService{
		resolveFn: func(_, nameOrARN string) (*handlers_iam.InstanceProfile, error) {
			calls++
			return &handlers_iam.InstanceProfile{ARN: nameOrARN, InstanceProfileID: "AIPAEXAMPLE"}, nil
		},
	}

	out := describeOutputWithProfiles(arn, arn, arn)
	EnrichInstanceProfileIDs(out, svc, "123456789012")

	assert.Equal(t, 1, calls, "three instances sharing one ARN should resolve once")
	for _, inst := range out.Reservations[0].Instances {
		assert.Equal(t, "AIPAEXAMPLE", aws.StringValue(inst.IamInstanceProfile.Id))
	}
}

func TestEnrichInstanceProfileIDs_ResolveErrorLeavesIdEmpty(t *testing.T) {
	failingARN := "arn:aws:iam::123456789012:instance-profile/missing"
	okARN := "arn:aws:iam::123456789012:instance-profile/present"
	svc := &fakeIAMService{
		resolveFn: func(_, nameOrARN string) (*handlers_iam.InstanceProfile, error) {
			if nameOrARN == failingARN {
				return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
			}
			return &handlers_iam.InstanceProfile{ARN: nameOrARN, InstanceProfileID: "AIPAOK"}, nil
		},
	}

	out := describeOutputWithProfiles(failingARN, okARN)
	EnrichInstanceProfileIDs(out, svc, "123456789012")

	assert.Nil(t, out.Reservations[0].Instances[0].IamInstanceProfile.Id,
		"failing lookup leaves Id nil")
	assert.Equal(t, "AIPAOK", aws.StringValue(out.Reservations[0].Instances[1].IamInstanceProfile.Id),
		"adjacent instances still enriched")
}

func TestEnrichInstanceProfileIDs_NoOpInputs(t *testing.T) {
	// Should not panic on nil out or nil iamSvc, and instances without a
	// profile attached are untouched.
	EnrichInstanceProfileIDs(nil, &fakeIAMService{}, "123456789012")

	out := describeOutputWithProfiles("arn:aws:iam::123456789012:instance-profile/foo")
	EnrichInstanceProfileIDs(out, nil, "123456789012")
	assert.Nil(t, out.Reservations[0].Instances[0].IamInstanceProfile.Id,
		"nil iamSvc is a no-op")

	noProfile := describeOutputWithProfiles("")
	EnrichInstanceProfileIDs(noProfile, &fakeIAMService{}, "123456789012")
	assert.Nil(t, noProfile.Reservations[0].Instances[0].IamInstanceProfile,
		"instance with no profile is untouched")
}
