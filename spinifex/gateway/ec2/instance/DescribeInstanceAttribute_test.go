package gateway_ec2_instance

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Validation tests ---

func TestValidateDescribeInstanceAttributeInput_NilInput(t *testing.T) {
	err := ValidateDescribeInstanceAttributeInput(nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

func TestValidateDescribeInstanceAttributeInput_MissingInstanceId(t *testing.T) {
	err := ValidateDescribeInstanceAttributeInput(&ec2.DescribeInstanceAttributeInput{
		Attribute: aws.String(ec2.InstanceAttributeNameInstanceType),
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDMalformed, err.Error())
}

func TestValidateDescribeInstanceAttributeInput_EmptyInstanceId(t *testing.T) {
	err := ValidateDescribeInstanceAttributeInput(&ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String(""),
		Attribute:  aws.String(ec2.InstanceAttributeNameInstanceType),
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDMalformed, err.Error())
}

func TestValidateDescribeInstanceAttributeInput_BadPrefix(t *testing.T) {
	err := ValidateDescribeInstanceAttributeInput(&ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String("x-12345"),
		Attribute:  aws.String(ec2.InstanceAttributeNameInstanceType),
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDMalformed, err.Error())
}

func TestValidateDescribeInstanceAttributeInput_MissingAttribute(t *testing.T) {
	err := ValidateDescribeInstanceAttributeInput(&ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String("i-abc123"),
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestValidateDescribeInstanceAttributeInput_EmptyAttribute(t *testing.T) {
	err := ValidateDescribeInstanceAttributeInput(&ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String("i-abc123"),
		Attribute:  aws.String(""),
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestValidateDescribeInstanceAttributeInput_Valid(t *testing.T) {
	err := ValidateDescribeInstanceAttributeInput(&ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String("i-abc123"),
		Attribute:  aws.String(ec2.InstanceAttributeNameInstanceType),
	})
	assert.NoError(t, err)
}

// --- Gateway function tests ---

// respondWithInstance returns a subscriber callback that emits a successful
// DescribeInstanceAttribute payload.
func respondWithInstance(t *testing.T, instanceID, instanceType string) nats.MsgHandler {
	return func(msg *nats.Msg) {
		var input ec2.DescribeInstanceAttributeInput
		require.NoError(t, json.Unmarshal(msg.Data, &input))
		out := &ec2.DescribeInstanceAttributeOutput{
			InstanceId:   aws.String(instanceID),
			InstanceType: &ec2.AttributeValue{Value: aws.String(instanceType)},
		}
		resp, _ := json.Marshal(out)
		msg.Respond(resp)
	}
}

// respondWithError returns a subscriber callback that emits a daemon-style
// ResponseError envelope (matches utils.GenerateErrorPayload).
func respondWithError(code string) nats.MsgHandler {
	return func(msg *nats.Msg) {
		msg.Respond(utils.GenerateErrorPayload(code))
	}
}

func TestDescribeInstanceAttribute_SingleNode(t *testing.T) {
	_, nc := startTestNATSServer(t)

	_, err := nc.Subscribe("ec2.DescribeInstanceAttribute", respondWithInstance(t, "i-test123", "t3.micro"))
	require.NoError(t, err)
	nc.Flush()

	input := &ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String("i-test123"),
		Attribute:  aws.String(ec2.InstanceAttributeNameInstanceType),
	}

	output, err := DescribeInstanceAttribute(context.Background(), input, nc, 1, "123456789012")
	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Equal(t, "i-test123", *output.InstanceId)
	assert.Equal(t, "t3.micro", *output.InstanceType.Value)
}

// TestDescribeInstanceAttribute_MultipleNodes_OwnerWins reproduces the
// regression that motivated the fan-out fix: in a 2-node cluster, only the
// owner has the instance in its local vmMgr/stoppedStore; the other returns
// InvalidInstanceID.NotFound. Pre-fix, queue-group routing made this fail
// ~50% of the time. Post-fix, the aggregator drops the NotFound and surfaces
// the success.
func TestDescribeInstanceAttribute_MultipleNodes_OwnerWins(t *testing.T) {
	_, nc := startTestNATSServer(t)

	_, err := nc.Subscribe("ec2.DescribeInstanceAttribute", respondWithError(awserrors.ErrorInvalidInstanceIDNotFound))
	require.NoError(t, err)

	nc2, err := nats.Connect(nc.ConnectedUrl())
	require.NoError(t, err)
	defer nc2.Close()
	_, err = nc2.Subscribe("ec2.DescribeInstanceAttribute", respondWithInstance(t, "i-owned", "t3.medium"))
	require.NoError(t, err)

	nc.Flush()
	nc2.Flush()

	input := &ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String("i-owned"),
		Attribute:  aws.String(ec2.InstanceAttributeNameInstanceType),
	}

	output, err := DescribeInstanceAttribute(context.Background(), input, nc, 2, "123456789012")
	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Equal(t, "i-owned", *output.InstanceId)
	assert.Equal(t, "t3.medium", *output.InstanceType.Value)
}

// TestDescribeInstanceAttribute_AllNodesNotFound covers the genuine missing
// instance case: every daemon confirms the instance is absent.
func TestDescribeInstanceAttribute_AllNodesNotFound(t *testing.T) {
	_, nc := startTestNATSServer(t)

	_, err := nc.Subscribe("ec2.DescribeInstanceAttribute", respondWithError(awserrors.ErrorInvalidInstanceIDNotFound))
	require.NoError(t, err)

	nc2, err := nats.Connect(nc.ConnectedUrl())
	require.NoError(t, err)
	defer nc2.Close()
	_, err = nc2.Subscribe("ec2.DescribeInstanceAttribute", respondWithError(awserrors.ErrorInvalidInstanceIDNotFound))
	require.NoError(t, err)

	nc.Flush()
	nc2.Flush()

	input := &ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String("i-missing"),
		Attribute:  aws.String(ec2.InstanceAttributeNameInstanceType),
	}

	_, err = DescribeInstanceAttribute(context.Background(), input, nc, 2, "123456789012")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}

// TestDescribeInstanceAttribute_ClientErrorPropagates ensures that a
// validation-class error (deterministic across daemons) is surfaced to the
// caller rather than getting masked by NotFound.
func TestDescribeInstanceAttribute_ClientErrorPropagates(t *testing.T) {
	_, nc := startTestNATSServer(t)

	_, err := nc.Subscribe("ec2.DescribeInstanceAttribute", respondWithError(awserrors.ErrorInvalidParameterValue))
	require.NoError(t, err)
	nc.Flush()

	input := &ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String("i-abc123"),
		Attribute:  aws.String(ec2.InstanceAttributeNameInstanceType),
	}

	_, err = DescribeInstanceAttribute(context.Background(), input, nc, 1, "123456789012")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

// TestDescribeInstanceAttribute_ServerErrorNotMaskedByNotFound ensures a node's
// 5xx fault (e.g. a transient KV outage on the owner) is surfaced rather than
// masked by sibling NotFound replies — otherwise terraform treats a live
// instance as deleted.
func TestDescribeInstanceAttribute_ServerErrorNotMaskedByNotFound(t *testing.T) {
	_, nc := startTestNATSServer(t)

	_, err := nc.Subscribe("ec2.DescribeInstanceAttribute", respondWithError(awserrors.ErrorServerInternal))
	require.NoError(t, err)

	nc2, err := nats.Connect(nc.ConnectedUrl())
	require.NoError(t, err)
	defer nc2.Close()
	_, err = nc2.Subscribe("ec2.DescribeInstanceAttribute", respondWithError(awserrors.ErrorInvalidInstanceIDNotFound))
	require.NoError(t, err)

	nc.Flush()
	nc2.Flush()

	input := &ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String("i-abc123"),
		Attribute:  aws.String(ec2.InstanceAttributeNameInstanceType),
	}

	_, err = DescribeInstanceAttribute(context.Background(), input, nc, 2, "123456789012")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

// TestDescribeInstanceAttribute_NoResponders returns NotFound (not a NATS
// timeout) so terraform retries cleanly rather than hanging.
func TestDescribeInstanceAttribute_NoResponders(t *testing.T) {
	_, nc := startTestNATSServer(t)

	input := &ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String("i-nowhere"),
		Attribute:  aws.String(ec2.InstanceAttributeNameInstanceType),
	}

	start := time.Now()
	_, err := DescribeInstanceAttribute(context.Background(), input, nc, 0, "123456789012")
	duration := time.Since(start)

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
	// Must not block the caller past the 3 s deadline.
	assert.Less(t, duration, 4*time.Second)
}

func TestDescribeInstanceAttribute_ValidationFailure(t *testing.T) {
	_, nc := startTestNATSServer(t)

	_, err := DescribeInstanceAttribute(context.Background(), nil, nc, 1, "123456789012")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}
