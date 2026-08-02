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

// subscribeEmptyInstanceBuckets makes the stopped/terminated KV bucket
// queries in gatherInstances succeed with no instances, so a fan-out that
// resolves fully still isn't marked incomplete just because the buckets have
// nothing to say.
func subscribeEmptyInstanceBuckets(t *testing.T, nc *nats.Conn) {
	t.Helper()
	for _, topic := range []string{"ec2.DescribeStoppedInstances", "ec2.DescribeTerminatedInstances"} {
		_, err := nc.Subscribe(topic, func(msg *nats.Msg) {
			data, _ := json.Marshal(&ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{}})
			msg.Respond(data)
		})
		require.NoError(t, err)
	}
	nc.Flush()
}

// TestDescribeInstancesChecked_ExplicitIDNotFound_CompleteSweep covers the
// bead's primary criterion: naming a nonexistent instance ID with a
// fully-answered fan-out returns InvalidInstanceID.NotFound instead of a
// silent empty list.
func TestDescribeInstancesChecked_ExplicitIDNotFound_CompleteSweep(t *testing.T) {
	_, nc := startTestNATSServer(t)
	subscribeEmptyInstanceBuckets(t, nc)

	_, err := nc.Subscribe("ec2.DescribeInstances", func(msg *nats.Msg) {
		data, _ := json.Marshal(&ec2.DescribeInstancesOutput{
			Reservations: []*ec2.Reservation{
				{
					ReservationId: aws.String("r-1"),
					Instances:     []*ec2.Instance{{InstanceId: aws.String("i-real")}},
				},
			},
		})
		msg.Respond(data)
	})
	require.NoError(t, err)
	nc.Flush()

	input := &ec2.DescribeInstancesInput{InstanceIds: []*string{aws.String("i-doesnotexist0000000")}}
	_, err = DescribeInstancesChecked(context.Background(), input, nc, 1, "123456789012")

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}

// TestDescribeInstances_ExplicitIDNotFound_StaysSilent guards the internal
// callers (IMDS lookup, RDS instance-state resolver, EKS control-plane
// reconciler) that call the plain DescribeInstances and rely on "not found"
// meaning an empty list, not an error. Only DescribeInstancesChecked — used
// solely by the gateway's customer-facing action — may assert NotFound.
func TestDescribeInstances_ExplicitIDNotFound_StaysSilent(t *testing.T) {
	_, nc := startTestNATSServer(t)
	subscribeEmptyInstanceBuckets(t, nc)

	// Mirrors a real node: it filters by the requested InstanceIds itself, so
	// a query for an ID it doesn't hold gets an empty reservations list back.
	_, err := nc.Subscribe("ec2.DescribeInstances", func(msg *nats.Msg) {
		data, _ := json.Marshal(&ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{}})
		msg.Respond(data)
	})
	require.NoError(t, err)
	nc.Flush()

	input := &ec2.DescribeInstancesInput{InstanceIds: []*string{aws.String("i-doesnotexist0000000")}}
	output, err := DescribeInstances(context.Background(), input, nc, 1, "123456789012")

	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Empty(t, output.Reservations)
}

// TestDescribeInstancesChecked_ExplicitIDFound_CompleteSweep is the positive
// counterpart: the same complete sweep, but the requested ID is present, so
// no error is raised.
func TestDescribeInstancesChecked_ExplicitIDFound_CompleteSweep(t *testing.T) {
	_, nc := startTestNATSServer(t)
	subscribeEmptyInstanceBuckets(t, nc)

	_, err := nc.Subscribe("ec2.DescribeInstances", func(msg *nats.Msg) {
		data, _ := json.Marshal(&ec2.DescribeInstancesOutput{
			Reservations: []*ec2.Reservation{
				{
					ReservationId: aws.String("r-1"),
					Instances:     []*ec2.Instance{{InstanceId: aws.String("i-real")}},
				},
			},
		})
		msg.Respond(data)
	})
	require.NoError(t, err)
	nc.Flush()

	input := &ec2.DescribeInstancesInput{InstanceIds: []*string{aws.String("i-real")}}
	output, err := DescribeInstancesChecked(context.Background(), input, nc, 1, "123456789012")

	require.NoError(t, err)
	require.Len(t, output.Reservations, 1)
}

// TestDescribeInstancesChecked_FilterOnlyNoMatch_ReturnsEmptyNotNotFound
// covers the bead's second criterion: a --filters query that matches nothing
// must still return rc=0 with an empty list, never NotFound (no instance IDs
// were named).
func TestDescribeInstancesChecked_FilterOnlyNoMatch_ReturnsEmptyNotNotFound(t *testing.T) {
	_, nc := startTestNATSServer(t)
	subscribeEmptyInstanceBuckets(t, nc)

	_, err := nc.Subscribe("ec2.DescribeInstances", func(msg *nats.Msg) {
		data, _ := json.Marshal(&ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{}})
		msg.Respond(data)
	})
	require.NoError(t, err)
	nc.Flush()

	input := &ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{{Name: aws.String("tag:nope"), Values: []*string{aws.String("nothing")}}},
	}
	output, err := DescribeInstancesChecked(context.Background(), input, nc, 1, "123456789012")

	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Empty(t, output.Reservations)
}

// TestDescribeInstancesChecked_ExplicitIDNotFound_NodeTimeout is the bead's
// subtle third criterion: a node timing out during the fan-out must not
// produce a false NotFound. Only one of two expected nodes answers, so the
// sweep is provably incomplete and the naive found-set check must be
// suppressed.
func TestDescribeInstancesChecked_ExplicitIDNotFound_NodeTimeout(t *testing.T) {
	_, nc := startTestNATSServer(t)
	subscribeEmptyInstanceBuckets(t, nc)

	// Only one of the two expected nodes has a subscriber; the second never
	// answers, so the fan-out hits its 3s deadline with sum.TimedOut=true.
	_, err := nc.Subscribe("ec2.DescribeInstances", func(msg *nats.Msg) {
		data, _ := json.Marshal(&ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{}})
		msg.Respond(data)
	})
	require.NoError(t, err)
	nc.Flush()

	input := &ec2.DescribeInstancesInput{InstanceIds: []*string{aws.String("i-on-the-slow-node")}}
	output, err := DescribeInstancesChecked(context.Background(), input, nc, 2, "123456789012")

	require.NoError(t, err, "an incomplete sweep must never assert a false NotFound")
	require.NotNil(t, output)
	assert.Empty(t, output.Reservations)
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
