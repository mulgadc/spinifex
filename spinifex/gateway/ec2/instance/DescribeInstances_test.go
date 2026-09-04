package gateway_ec2_instance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	_, nc := startTestNATSServer(t)

	input := &ec2.DescribeInstancesInput{}
	output, err := DescribeInstances(context.Background(), input, nc, 0, "123456789012")

	// No subscribers means timeout — function returns empty reservations, no error
	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Empty(t, output.Reservations)
}

func TestDescribeInstances_NodeReturnsError(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	_, nc := startTestNATSServer(t)

	// Node responds after a delay (but within timeout)
	nc.Subscribe("ec2.DescribeInstances", func(msg *nats.Msg) {
		time.Sleep(50 * time.Millisecond)
		data, _ := json.Marshal(&ec2.DescribeInstancesOutput{
			Reservations: []*ec2.Reservation{
				{ReservationId: aws.String("r-delayed")},
			},
		})
		msg.Respond(data)
	})

	input := &ec2.DescribeInstancesInput{}
	// 0 expected nodes = timeout-based collection: the whole window is waited out.
	output, err := DescribeInstances(context.Background(), input, nc, 0, "123456789012",
		WithFanoutTimeout(300*time.Millisecond))

	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Len(t, output.Reservations, 1)
}

func TestDescribeInstances_EarlyExitWithExpectedNodes(t *testing.T) {
	t.Parallel()
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
		subscribeOKInstanceBucket(t, nc, topic)
	}
	nc.Flush()
}

// subscribeOKInstanceBucket answers one bucket topic with an empty, successful
// result.
func subscribeOKInstanceBucket(t *testing.T, nc *nats.Conn, topic string) {
	t.Helper()
	_, err := nc.Subscribe(topic, func(msg *nats.Msg) {
		data, _ := json.Marshal(&ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{}})
		msg.Respond(data)
	})
	require.NoError(t, err)
}

// subscribeFailingInstanceBucket answers one bucket topic with an error
// envelope, so queryInstanceBucket's ok return is false without paying its
// 3s request timeout.
func subscribeFailingInstanceBucket(t *testing.T, nc *nats.Conn, topic string) {
	t.Helper()
	_, err := nc.Subscribe(topic, func(msg *nats.Msg) {
		msg.Respond(utils.GenerateErrorPayload(awserrors.ErrorInternalError))
	})
	require.NoError(t, err)
}

// TestDescribeInstancesChecked_ExplicitIDNotFound_CompleteSweep covers the
// The primary criterion: naming a nonexistent instance ID with a
// fully-answered fan-out returns InvalidInstanceID.NotFound instead of a
// silent empty list.
func TestDescribeInstancesChecked_ExplicitIDNotFound_CompleteSweep(t *testing.T) {
	t.Parallel()
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
	_, err = DescribeInstancesChecked(context.Background(), input, nc, 1, nil, "123456789012")

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}

// TestDescribeInstances_ExplicitIDNotFound_StaysSilent guards the internal
// callers (IMDS lookup, RDS instance-state resolver, EKS control-plane
// reconciler) that call the plain DescribeInstances and rely on "not found"
// meaning an empty list, not an error. Only DescribeInstancesChecked — used
// solely by the gateway's customer-facing action — may assert NotFound.
func TestDescribeInstances_ExplicitIDNotFound_StaysSilent(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	output, err := DescribeInstancesChecked(context.Background(), input, nc, 1, nil, "123456789012")

	require.NoError(t, err)
	require.Len(t, output.Reservations, 1)
}

// TestDescribeInstancesChecked_FilterOnlyNoMatch_ReturnsEmptyNotNotFound
// covers the second criterion: a --filters query that matches nothing
// must still return rc=0 with an empty list, never NotFound (no instance IDs
// were named).
func TestDescribeInstancesChecked_FilterOnlyNoMatch_ReturnsEmptyNotNotFound(t *testing.T) {
	t.Parallel()
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
	output, err := DescribeInstancesChecked(context.Background(), input, nc, 1, nil, "123456789012")

	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Empty(t, output.Reservations)
}

// TestDescribeInstancesChecked_ExplicitIDNotFound_NodeTimeout verifies the
// subtle third criterion: a node timing out during the fan-out must not
// produce a false NotFound. Only one of two expected nodes answers, so the
// sweep is provably incomplete — and since the ID was explicitly named, the
// answer is a retryable ServiceUnavailable rather than a silent empty 200:
// an incomplete sweep cannot prove absence, so it must not be reported as if
// it had.
func TestDescribeInstancesChecked_ExplicitIDNotFound_NodeTimeout(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeEmptyInstanceBuckets(t, nc)

	// Only one of the two expected nodes has a subscriber; the second never
	// answers, so the fan-out hits its deadline with sum.TimedOut=true.
	_, err := nc.Subscribe("ec2.DescribeInstances", func(msg *nats.Msg) {
		data, _ := json.Marshal(&ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{}})
		msg.Respond(data)
	})
	require.NoError(t, err)
	nc.Flush()

	input := &ec2.DescribeInstancesInput{InstanceIds: []*string{aws.String("i-on-the-slow-node")}}
	output, err := DescribeInstancesChecked(context.Background(), input, nc, 2, nil, "123456789012",
		WithFanoutTimeout(300*time.Millisecond))

	require.Error(t, err, "an incomplete sweep must never assert a false NotFound, but must not stay silent either")
	assert.Equal(t, awserrors.ErrorServiceUnavailable, err.Error())
	assert.False(t, awserrors.IsTerminal(err), "the partiality signal must be retryable, not terminal")
	assert.Nil(t, output)
}

// --- Identity-mode completeness (nodeIDs set) ---

// A missing configured node suppresses NotFound even though the requested ID
// never showed up in what did arrive: the sweep is provably incomplete, so a
// retryable error is the only safe answer for a named ID.
func TestDescribeInstancesChecked_Identity_MissingNodeSuppressesNotFound(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeEmptyInstanceBuckets(t, nc)

	data, _ := json.Marshal(&ec2.DescribeInstancesOutput{
		Reservations: []*ec2.Reservation{{
			ReservationId: aws.String("r-1"),
			Instances:     []*ec2.Instance{{InstanceId: aws.String("i-real")}},
		}},
	})
	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-1", data)
	// node-2 is configured but never answers.

	input := &ec2.DescribeInstancesInput{InstanceIds: []*string{aws.String("i-doesnotexist0000000")}}
	output, err := DescribeInstancesChecked(context.Background(), input, nc, 0, []string{"node-1", "node-2"}, "123456789012",
		WithFanoutTimeout(300*time.Millisecond))

	require.Error(t, err, "a node missing from the configured set must never assert a false NotFound")
	assert.Equal(t, awserrors.ErrorServiceUnavailable, err.Error())
	assert.Nil(t, output)
}

// The positive counterpart: every configured node answers, so the sweep is
// complete and the assertion fires for a genuinely absent instance ID.
func TestDescribeInstancesChecked_Identity_AllNodesAnswer_NotFoundAsserted(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeEmptyInstanceBuckets(t, nc)

	data, _ := json.Marshal(&ec2.DescribeInstancesOutput{
		Reservations: []*ec2.Reservation{{
			ReservationId: aws.String("r-1"),
			Instances:     []*ec2.Instance{{InstanceId: aws.String("i-real")}},
		}},
	})
	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-1", data)
	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-2",
		mustMarshalDescribeOutput(t, &ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{}}))

	input := &ec2.DescribeInstancesInput{InstanceIds: []*string{aws.String("i-doesnotexist0000000")}}
	_, err := DescribeInstancesChecked(context.Background(), input, nc, 0, []string{"node-1", "node-2"}, "123456789012",
		WithFanoutTimeout(2*time.Second))

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}

// A node that answers with a decodable error envelope (a 5xx from that node)
// never becomes a valid responder, so it counts the same as a missing node —
// the sweep stays incomplete and NotFound is suppressed in favour of a
// retryable error.
func TestDescribeInstancesChecked_Identity_ErroringNodeSuppressesNotFound(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeEmptyInstanceBuckets(t, nc)

	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-1",
		mustMarshalDescribeOutput(t, &ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{}}))
	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-2",
		utils.GenerateErrorPayload(awserrors.ErrorBandwidthLimitExceeded))

	input := &ec2.DescribeInstancesInput{InstanceIds: []*string{aws.String("i-doesnotexist0000000")}}
	output, err := DescribeInstancesChecked(context.Background(), input, nc, 0, []string{"node-1", "node-2"}, "123456789012",
		WithFanoutTimeout(300*time.Millisecond))

	require.Error(t, err, "an erroring node must suppress NotFound, not be treated as an empty-handed valid responder")
	assert.Equal(t, awserrors.ErrorServiceUnavailable, err.Error())
	assert.Nil(t, output)
}

// A payload that is neither a valid error envelope nor a decodable
// DescribeInstancesOutput (a malformed reply) leaves its node out of
// ValidResponders, so the sweep is incomplete and NotFound is suppressed —
// the same as a node that never answered at all — in favour of a retryable
// error.
func TestDescribeInstancesChecked_Identity_UndecodablePayloadSuppressesNotFound(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeEmptyInstanceBuckets(t, nc)

	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-1",
		mustMarshalDescribeOutput(t, &ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{}}))
	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-2", []byte("not valid json at all"))

	input := &ec2.DescribeInstancesInput{InstanceIds: []*string{aws.String("i-doesnotexist0000000")}}
	output, err := DescribeInstancesChecked(context.Background(), input, nc, 0, []string{"node-1", "node-2"}, "123456789012",
		WithFanoutTimeout(300*time.Millisecond))

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServiceUnavailable, err.Error())
	assert.Nil(t, output)
}

// A node with nil Reservations still decoded successfully, so it is a valid
// responder — this is the trap the plan calls out explicitly: nil
// Reservations is not the same thing as a failed decode.
func TestDescribeInstancesChecked_Identity_NilReservationsIsValidResponder(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeEmptyInstanceBuckets(t, nc)

	// {} decodes to DescribeInstancesOutput{Reservations: nil}.
	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-1", []byte(`{}`))
	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-2", []byte(`{}`))

	input := &ec2.DescribeInstancesInput{InstanceIds: []*string{aws.String("i-doesnotexist0000000")}}
	_, err := DescribeInstancesChecked(context.Background(), input, nc, 0, []string{"node-1", "node-2"}, "123456789012",
		WithFanoutTimeout(2*time.Second))

	require.Error(t, err, "a nil-Reservations node is a complete, valid responder, so the sweep is complete and NotFound must fire")
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}

// An empty or unset nodeIDs falls back to the legacy expectedNodes path.
// DescribeInstancesChecked's only production caller always passes
// expectedNodes=0 alongside its node set, so an empty node set (e.g. an
// unconfigured cluster) can never satisfy expectedNodes>0 and NotFound stays
// suppressed — the fallback fails closed rather than trusting a stale count.
func TestDescribeInstancesChecked_Identity_EmptyNodeIDsFallsBackAndStaysSuppressed(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeEmptyInstanceBuckets(t, nc)

	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-1",
		mustMarshalDescribeOutput(t, &ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{}}))

	input := &ec2.DescribeInstancesInput{InstanceIds: []*string{aws.String("i-doesnotexist0000000")}}
	output, err := DescribeInstancesChecked(context.Background(), input, nc, 0, nil, "123456789012",
		WithFanoutTimeout(300*time.Millisecond))

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServiceUnavailable, err.Error())
	assert.Nil(t, output)
}

// mustMarshalDescribeOutput is a t.Helper wrapper so the identity-mode tests
// above stay one line each instead of repeating the error check.
func mustMarshalDescribeOutput(t *testing.T, out *ec2.DescribeInstancesOutput) []byte {
	t.Helper()
	data, err := json.Marshal(out)
	require.NoError(t, err)
	return data
}

// captureLogs redirects the default slog logger into a buffer for the
// duration of a test. Not run under t.Parallel(): slog.Default() is a
// package global, and this test needs it undisturbed by other tests' output.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// An unattributable responder (a frame with no node ID header) makes identity
// completeness permanently unsatisfiable — the missing/present-node judgement
// this whole mode exists for never gets an answer once responder identity is
// untrustworthy. Without a log there is no way to tell that failure mode
// apart from a well-behaved sweep after the fact, so the log line is the
// contract under test.
func TestDescribeInstancesChecked_Identity_UnidentifiedFrameLogsWarning(t *testing.T) {
	_, nc := startTestNATSServer(t)
	subscribeEmptyInstanceBuckets(t, nc)
	logs := captureLogs(t)

	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-1",
		mustMarshalDescribeOutput(t, &ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{}}))
	// node-2 answers, but with no X-Node-ID header — unattributable.
	_, err := nc.Subscribe("ec2.DescribeInstances", func(msg *nats.Msg) {
		_ = msg.Respond(mustMarshalDescribeOutput(t, &ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{}}))
	})
	require.NoError(t, err)

	input := &ec2.DescribeInstancesInput{InstanceIds: []*string{aws.String("i-doesnotexist0000000")}}
	_, err = DescribeInstancesChecked(context.Background(), input, nc, 0, []string{"node-1", "node-2"}, "123456789012",
		WithFanoutTimeout(300*time.Millisecond))
	require.Error(t, err, "an unidentified frame suppresses NotFound, but a named ID still needs a retryable answer")
	assert.Equal(t, awserrors.ErrorServiceUnavailable, err.Error())

	out := logs.String()
	assert.Contains(t, out, "no node ID header")
	assert.Contains(t, out, "unidentified_frames=1")
}

func TestDescribeInstances_ClosedConnection(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// frameWithInstances builds one daemon reply carrying the given instance IDs.
func frameWithInstances(t *testing.T, nodeID string, instanceIDs ...string) utils.Frame {
	t.Helper()
	instances := make([]*ec2.Instance, 0, len(instanceIDs))
	for _, id := range instanceIDs {
		instances = append(instances, &ec2.Instance{InstanceId: aws.String(id)})
	}
	data, err := json.Marshal(ec2.DescribeInstancesOutput{
		Reservations: []*ec2.Reservation{{Instances: instances}},
	})
	require.NoError(t, err)
	return utils.Frame{NodeID: nodeID, Data: data}
}

// reservationsWith builds the bucket-query shape the settler observes.
func reservationsWith(instanceIDs ...string) []*ec2.Reservation {
	instances := make([]*ec2.Instance, 0, len(instanceIDs))
	for _, id := range instanceIDs {
		instances = append(instances, &ec2.Instance{InstanceId: aws.String(id)})
	}
	return []*ec2.Reservation{{Instances: instances}}
}

func TestInstanceSettler_SettlesOnlyWhenEveryIDIsSeen(t *testing.T) {
	t.Parallel()
	settler := newInstanceSettler([]*string{aws.String("i-a"), aws.String("i-b")})

	frames := []utils.Frame{frameWithInstances(t, "node-1", "i-a")}
	assert.False(t, settler.settled(frames, utils.Summary{}), "one of two ids found is not settled")

	frames = append(frames, frameWithInstances(t, "node-2", "i-b"))
	assert.True(t, settler.settled(frames, utils.Summary{}), "both ids found, nothing left to prove")
}

// An id that no node holds must never settle, so absence keeps paying the full
// deadline — that is the whole reason CollectUntilDeadline exists.
func TestInstanceSettler_AbsentIDNeverSettles(t *testing.T) {
	t.Parallel()
	settler := newInstanceSettler([]*string{aws.String("i-missing")})

	frames := []utils.Frame{
		frameWithInstances(t, "node-1", "i-other"),
		frameWithInstances(t, "node-2", "i-another"),
	}
	assert.False(t, settler.settled(frames, utils.Summary{}))
}

func TestInstanceSettler_UndecodableFrameDoesNotSettle(t *testing.T) {
	t.Parallel()
	settler := newInstanceSettler([]*string{aws.String("i-a")})

	frames := []utils.Frame{{NodeID: "node-1", Data: []byte("not json")}}
	assert.False(t, settler.settled(frames, utils.Summary{}))

	frames = append(frames, frameWithInstances(t, "node-2", "i-a"))
	assert.True(t, settler.settled(frames, utils.Summary{}), "a later decodable frame still settles")
}

// A stopped or terminated instance never appears in a fan-out frame, so the
// bucket queries have to be able to settle the collection on their own.
func TestInstanceSettler_BucketHitAloneSettles(t *testing.T) {
	t.Parallel()
	settler := newInstanceSettler([]*string{aws.String("i-stopped")})

	frames := []utils.Frame{frameWithInstances(t, "node-1", "i-running")}
	assert.False(t, settler.settled(frames, utils.Summary{}))

	settler.observe(reservationsWith("i-stopped"))
	assert.True(t, settler.settled(frames, utils.Summary{}), "the bucket answered, so nothing is outstanding")
}

func TestInstanceSettler_NoIDsSettlesImmediately(t *testing.T) {
	t.Parallel()
	assert.True(t, newInstanceSettler(nil).settled(nil, utils.Summary{}))
	assert.True(t, newInstanceSettler([]*string{nil}).settled(nil, utils.Summary{}))
}

// observe races settled in production: the bucket goroutines write while Gather
// reads. Run under -race to make the lock earn its place.
func TestInstanceSettler_ConcurrentObserveAndSettled(t *testing.T) {
	t.Parallel()
	settler := newInstanceSettler([]*string{aws.String("i-a"), aws.String("i-b")})
	frames := []utils.Frame{frameWithInstances(t, "node-1", "i-a")}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		settler.observe(reservationsWith("i-b"))
	}()
	go func() {
		defer wg.Done()
		settler.settled(frames, utils.Summary{})
	}()
	wg.Wait()

	assert.True(t, settler.settled(frames, utils.Summary{}))
}

func TestAcquireAbsenceProofSlot_ThrottlesPastTheLimitAndRecovers(t *testing.T) {
	releases := make([]func(), 0, maxConcurrentAbsenceProofs)
	t.Cleanup(func() {
		for _, release := range releases {
			release()
		}
	})

	for i := range maxConcurrentAbsenceProofs {
		release, ok := acquireAbsenceProofSlot()
		require.True(t, ok, "slot %d of the budget must be available", i)
		releases = append(releases, release)
	}

	_, ok := acquireAbsenceProofSlot()
	assert.False(t, ok, "the budget must refuse rather than queue once exhausted")

	releases[0]()
	releases = releases[1:]
	regained, ok := acquireAbsenceProofSlot()
	require.True(t, ok, "releasing a slot must return it to the budget")
	releases = append(releases, regained)
}

// The throttle decision is made before any reply is read, so it cannot vary
// with whether the instance exists — otherwise it would reintroduce the timing
// oracle the full deadline exists to close.
func TestAcquireAbsenceProofSlot_DoesNotBlockWhenExhausted(t *testing.T) {
	releases := make([]func(), 0, maxConcurrentAbsenceProofs)
	t.Cleanup(func() {
		for _, release := range releases {
			release()
		}
	})
	for range maxConcurrentAbsenceProofs {
		release, ok := acquireAbsenceProofSlot()
		require.True(t, ok)
		releases = append(releases, release)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = acquireAbsenceProofSlot()
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("acquireAbsenceProofSlot blocked; it must refuse immediately so the backlog cannot grow")
	}
}

// --- Task 2: the partiality signal ---

// A --filters-only query never asserts absence, complete sweep or not: a
// partial listing is a legitimate answer to a listing question. Only one of
// two expected nodes answers, so the sweep is incomplete, and the call must
// still return the partial data with no error.
func TestDescribeInstancesChecked_FilterOnlyIncompleteSweep_StaysPartialNoError(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeEmptyInstanceBuckets(t, nc)

	_, err := nc.Subscribe("ec2.DescribeInstances", func(msg *nats.Msg) {
		data, _ := json.Marshal(&ec2.DescribeInstancesOutput{
			Reservations: []*ec2.Reservation{{
				ReservationId: aws.String("r-partial"),
				Instances:     []*ec2.Instance{{InstanceId: aws.String("i-partial")}},
			}},
		})
		msg.Respond(data)
	})
	require.NoError(t, err)
	nc.Flush()

	input := &ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{{Name: aws.String("tag:env"), Values: []*string{aws.String("prod")}}},
	}
	output, err := DescribeInstancesChecked(context.Background(), input, nc, 2, nil, "123456789012",
		WithFanoutTimeout(300*time.Millisecond))

	require.NoError(t, err, "a filters-only query must never assert absence, complete or not")
	require.NotNil(t, output)
	assert.Len(t, output.Reservations, 1)
}

// --- Task 1: separated bucket vetoes ---

// A stopped-bucket failure alone must veto completeness even though the
// fan-out and the terminated bucket both succeed, and it must be
// distinguishable — via a dedicated failure on that source — from a
// terminated-bucket failure below.
func TestDescribeInstancesForReconcile_StoppedBucketFailure_Incomplete(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeFailingInstanceBucket(t, nc, "ec2.DescribeStoppedInstances")
	subscribeOKInstanceBucket(t, nc, "ec2.DescribeTerminatedInstances")
	nc.Flush()

	_, err := nc.Subscribe("ec2.DescribeInstances", func(msg *nats.Msg) {
		data, _ := json.Marshal(&ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{}})
		msg.Respond(data)
	})
	require.NoError(t, err)
	nc.Flush()

	input := &ec2.DescribeInstancesInput{}
	_, complete, err := DescribeInstancesForReconcile(context.Background(), input, nc, 1, "123456789012")

	require.NoError(t, err)
	assert.False(t, complete, "a failed stopped-bucket query alone must veto completeness")
}

// The terminated-bucket mirror of the test above: this source failing, with
// the stopped bucket and the fan-out both succeeding, must independently veto
// completeness the same way.
func TestDescribeInstancesForReconcile_TerminatedBucketFailure_Incomplete(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeOKInstanceBucket(t, nc, "ec2.DescribeStoppedInstances")
	subscribeFailingInstanceBucket(t, nc, "ec2.DescribeTerminatedInstances")
	nc.Flush()

	_, err := nc.Subscribe("ec2.DescribeInstances", func(msg *nats.Msg) {
		data, _ := json.Marshal(&ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{}})
		msg.Respond(data)
	})
	require.NoError(t, err)
	nc.Flush()

	input := &ec2.DescribeInstancesInput{}
	_, complete, err := DescribeInstancesForReconcile(context.Background(), input, nc, 1, "123456789012")

	require.NoError(t, err)
	assert.False(t, complete, "a failed terminated-bucket query alone must veto completeness")
}

// The positive control for the two tests above: with both buckets and the
// fan-out all succeeding, the sweep is complete. Without this, a mutation
// that always reports incomplete would pass both failure tests undetected.
func TestDescribeInstancesForReconcile_BothBucketsOK_Complete(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeEmptyInstanceBuckets(t, nc)

	_, err := nc.Subscribe("ec2.DescribeInstances", func(msg *nats.Msg) {
		data, _ := json.Marshal(&ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{}})
		msg.Respond(data)
	})
	require.NoError(t, err)
	nc.Flush()

	input := &ec2.DescribeInstancesInput{}
	_, complete, err := DescribeInstancesForReconcile(context.Background(), input, nc, 1, "123456789012")

	require.NoError(t, err)
	assert.True(t, complete)
}

// A stopped-bucket failure alone, surfaced through the customer-facing
// variant with a named ID that is genuinely absent from the fan-out, must
// suppress the 404 in favour of the new retryable signal — never a silent
// empty 200 and never a false NotFound.
func TestDescribeInstancesChecked_StoppedBucketFailure_SuppressesNotFound(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeFailingInstanceBucket(t, nc, "ec2.DescribeStoppedInstances")
	subscribeOKInstanceBucket(t, nc, "ec2.DescribeTerminatedInstances")
	nc.Flush()

	_, err := nc.Subscribe("ec2.DescribeInstances", func(msg *nats.Msg) {
		data, _ := json.Marshal(&ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{}})
		msg.Respond(data)
	})
	require.NoError(t, err)
	nc.Flush()

	input := &ec2.DescribeInstancesInput{InstanceIds: []*string{aws.String("i-doesnotexist0000000")}}
	output, err := DescribeInstancesChecked(context.Background(), input, nc, 1, nil, "123456789012")

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServiceUnavailable, err.Error())
	assert.Nil(t, output)
}

// The terminated-bucket mirror of the test above.
func TestDescribeInstancesChecked_TerminatedBucketFailure_SuppressesNotFound(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeOKInstanceBucket(t, nc, "ec2.DescribeStoppedInstances")
	subscribeFailingInstanceBucket(t, nc, "ec2.DescribeTerminatedInstances")
	nc.Flush()

	_, err := nc.Subscribe("ec2.DescribeInstances", func(msg *nats.Msg) {
		data, _ := json.Marshal(&ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{}})
		msg.Respond(data)
	})
	require.NoError(t, err)
	nc.Flush()

	input := &ec2.DescribeInstancesInput{InstanceIds: []*string{aws.String("i-doesnotexist0000000")}}
	output, err := DescribeInstancesChecked(context.Background(), input, nc, 1, nil, "123456789012")

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServiceUnavailable, err.Error())
	assert.Nil(t, output)
}

// --- Untouched paths that must survive this stage unchanged ---

// The forwarded node-4xx path takes precedence over the new partiality
// signal: when a node reports a deterministic 4xx and nothing else was
// collected, that code is returned as-is, even for an explicit-ID request
// that would otherwise reach the retryable-error branch.
func TestDescribeInstancesChecked_ForwardedNodeClientError_TakesPrecedence(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeEmptyInstanceBuckets(t, nc)

	_, err := nc.Subscribe("ec2.DescribeInstances", func(msg *nats.Msg) {
		msg.Respond(utils.GenerateErrorPayload(awserrors.ErrorInvalidParameterValue))
	})
	require.NoError(t, err)
	nc.Flush()

	input := &ec2.DescribeInstancesInput{InstanceIds: []*string{aws.String("i-doesnotexist0000000")}}
	output, err := DescribeInstancesChecked(context.Background(), input, nc, 1, nil, "123456789012")

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error(),
		"a forwarded node 4xx must not be swallowed by the new retryable-error branch")
	assert.Nil(t, output)
}

// The absence-proof semaphore still throttles the full DescribeInstancesChecked
// path, not just the acquireAbsenceProofSlot primitive: with every slot held,
// an explicit-ID identity-mode call returns RequestLimitExceeded rather than
// blocking or falling through to the new partiality signal.
func TestDescribeInstancesChecked_AbsenceProofThrottle_RequestLimitExceeded(t *testing.T) {
	_, nc := startTestNATSServer(t)
	subscribeEmptyInstanceBuckets(t, nc)
	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-1",
		mustMarshalDescribeOutput(t, &ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{}}))

	releases := make([]func(), 0, maxConcurrentAbsenceProofs)
	t.Cleanup(func() {
		for _, release := range releases {
			release()
		}
	})
	for range maxConcurrentAbsenceProofs {
		release, ok := acquireAbsenceProofSlot()
		require.True(t, ok)
		releases = append(releases, release)
	}

	input := &ec2.DescribeInstancesInput{InstanceIds: []*string{aws.String("i-doesnotexist0000000")}}
	output, err := DescribeInstancesChecked(context.Background(), input, nc, 0, []string{"node-1"}, "123456789012",
		WithFanoutTimeout(300*time.Millisecond))

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorRequestLimitExceeded, err.Error())
	assert.Nil(t, output)
}
