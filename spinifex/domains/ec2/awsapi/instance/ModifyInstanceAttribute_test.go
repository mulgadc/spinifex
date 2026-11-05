package instance

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Validation tests ---

func TestValidateModifyInstanceAttributeInput_NilInput(t *testing.T) {
	t.Parallel()
	err := ValidateModifyInstanceAttributeInput(nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

func TestValidateModifyInstanceAttributeInput_MissingInstanceId(t *testing.T) {
	t.Parallel()
	err := ValidateModifyInstanceAttributeInput(&ec2.ModifyInstanceAttributeInput{})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDMalformed, err.Error())
}

func TestValidateModifyInstanceAttributeInput_EmptyInstanceId(t *testing.T) {
	t.Parallel()
	err := ValidateModifyInstanceAttributeInput(&ec2.ModifyInstanceAttributeInput{
		InstanceId: aws.String(""),
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDMalformed, err.Error())
}

func TestValidateModifyInstanceAttributeInput_BadPrefix(t *testing.T) {
	t.Parallel()
	err := ValidateModifyInstanceAttributeInput(&ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String("x-12345"),
		InstanceType: &ec2.AttributeValue{Value: aws.String("t3.micro")},
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDMalformed, err.Error())
}

func assertSingleAttributeError(t *testing.T, err error, wantMessage string) {
	t.Helper()
	code, message, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorInvalidParameterCombination, code)
	assert.Equal(t, wantMessage, message)
}

func TestValidateModifyInstanceAttributeInput_NoAttributeSet(t *testing.T) {
	t.Parallel()
	err := ValidateModifyInstanceAttributeInput(&ec2.ModifyInstanceAttributeInput{
		InstanceId: aws.String("i-abc123"),
	})
	assertSingleAttributeError(t, err, "No attributes specified.")
}

func TestValidateModifyInstanceAttributeInput_MultipleAttributes(t *testing.T) {
	t.Parallel()
	err := ValidateModifyInstanceAttributeInput(&ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String("i-abc123"),
		InstanceType: &ec2.AttributeValue{Value: aws.String("t3.micro")},
		UserData:     &ec2.BlobAttributeValue{Value: []byte("data")},
	})
	assertSingleAttributeError(t, err, "The request must contain a single attribute")
}

func TestValidateModifyInstanceAttributeInput_EmptyInstanceType(t *testing.T) {
	t.Parallel()
	err := ValidateModifyInstanceAttributeInput(&ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String("i-abc123"),
		InstanceType: &ec2.AttributeValue{Value: aws.String("")},
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceAttributeValue, err.Error())
}

func TestValidateModifyInstanceAttributeInput_NilInstanceTypeValue(t *testing.T) {
	t.Parallel()
	err := ValidateModifyInstanceAttributeInput(&ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String("i-abc123"),
		InstanceType: &ec2.AttributeValue{},
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceAttributeValue, err.Error())
}

func TestValidateModifyInstanceAttributeInput_ValidInstanceType(t *testing.T) {
	t.Parallel()
	err := ValidateModifyInstanceAttributeInput(&ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String("i-abc123"),
		InstanceType: &ec2.AttributeValue{Value: aws.String("t3.medium")},
	})
	assert.NoError(t, err)
}

func TestValidateModifyInstanceAttributeInput_ValidUserData(t *testing.T) {
	t.Parallel()
	err := ValidateModifyInstanceAttributeInput(&ec2.ModifyInstanceAttributeInput{
		InstanceId: aws.String("i-abc123"),
		UserData:   &ec2.BlobAttributeValue{Value: []byte("IyEvYmluL2Jhc2g=")},
	})
	assert.NoError(t, err)
}

func TestValidateModifyInstanceAttributeInput_ValidDisableApiTermination(t *testing.T) {
	t.Parallel()
	err := ValidateModifyInstanceAttributeInput(&ec2.ModifyInstanceAttributeInput{
		InstanceId:            aws.String("i-abc123"),
		DisableApiTermination: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	})
	assert.NoError(t, err)
}

func TestValidateModifyInstanceAttributeInput_DisableApiTerminationWithOther(t *testing.T) {
	t.Parallel()
	err := ValidateModifyInstanceAttributeInput(&ec2.ModifyInstanceAttributeInput{
		InstanceId:            aws.String("i-abc123"),
		DisableApiTermination: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
		InstanceType:          &ec2.AttributeValue{Value: aws.String("t3.micro")},
	})
	assertSingleAttributeError(t, err, "The request must contain a single attribute")
}

func TestValidateModifyInstanceAttributeInput_ValidSourceDestCheck(t *testing.T) {
	t.Parallel()
	err := ValidateModifyInstanceAttributeInput(&ec2.ModifyInstanceAttributeInput{
		InstanceId:      aws.String("i-abc123"),
		SourceDestCheck: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	})
	assert.NoError(t, err)
}

func TestValidateModifyInstanceAttributeInput_SourceDestCheckWithOtherAttribute(t *testing.T) {
	t.Parallel()
	err := ValidateModifyInstanceAttributeInput(&ec2.ModifyInstanceAttributeInput{
		InstanceId:      aws.String("i-abc123"),
		SourceDestCheck: &ec2.AttributeBooleanValue{Value: aws.Bool(false)},
		InstanceType:    &ec2.AttributeValue{Value: aws.String("t3.micro")},
	})
	assertSingleAttributeError(t, err, "The request must contain a single attribute")
}

func TestValidateModifyInstanceAttributeInput_SourceDestCheckWithDisableApiTermination(t *testing.T) {
	t.Parallel()
	err := ValidateModifyInstanceAttributeInput(&ec2.ModifyInstanceAttributeInput{
		InstanceId:            aws.String("i-abc123"),
		SourceDestCheck:       &ec2.AttributeBooleanValue{Value: aws.Bool(false)},
		DisableApiTermination: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	})
	assertSingleAttributeError(t, err, "The request must contain a single attribute")
}

// --- Gateway function tests ---

func TestModifyInstanceAttribute_Success(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	nc.QueueSubscribe("ec2.ModifyInstanceAttribute", "spinifex-workers", func(msg *nats.Msg) {
		var input ec2.ModifyInstanceAttributeInput
		err := json.Unmarshal(msg.Data, &input)
		require.NoError(t, err)
		assert.Equal(t, "i-test123", *input.InstanceId)
		msg.Respond([]byte(`{}`))
	})

	input := &ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String("i-test123"),
		InstanceType: &ec2.AttributeValue{Value: aws.String("t3.medium")},
	}

	_, err := ModifyInstanceAttribute(context.Background(), input, nc, "123456789012")
	assert.NoError(t, err)
}

func TestModifyInstanceAttribute_SourceDestCheck(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	nc.QueueSubscribe("ec2.ModifyInstanceAttribute", "spinifex-workers", func(msg *nats.Msg) {
		msg.Respond([]byte(`{}`))
	})

	input := &ec2.ModifyInstanceAttributeInput{
		InstanceId:      aws.String("i-test123"),
		SourceDestCheck: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	}

	_, err := ModifyInstanceAttribute(context.Background(), input, nc, "123456789012")
	assert.NoError(t, err)
}

func TestModifyInstanceAttribute_DaemonError(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	nc.QueueSubscribe("ec2.ModifyInstanceAttribute", "spinifex-workers", func(msg *nats.Msg) {
		msg.Respond([]byte(`{"Code":"InvalidInstanceID.NotFound"}`))
	})

	input := &ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String("i-notfound"),
		InstanceType: &ec2.AttributeValue{Value: aws.String("t3.medium")},
	}

	_, err := ModifyInstanceAttribute(context.Background(), input, nc, "123456789012")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}

func TestModifyInstanceAttribute_ValidationFailure(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	_, err := ModifyInstanceAttribute(context.Background(), nil, nc, "123456789012")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

func TestModifyInstanceAttribute_DisableApiTerminationRouting(t *testing.T) {
	const id = "i-0123456789abcdef0"
	_, nc := startTestNATSServer(t)

	var gotValues []bool
	var mu sync.Mutex
	_, err := nc.Subscribe(ec2v1.InstanceCommandSubject(id), func(msg *nats.Msg) {
		var cmd ec2v1.EC2InstanceCommand
		if json.Unmarshal(msg.Data, &cmd) == nil && cmd.Attributes.SetDisableApiTermination && cmd.DisableApiTerminationData != nil {
			mu.Lock()
			gotValues = append(gotValues, cmd.DisableApiTerminationData.Value)
			mu.Unlock()
		}
		reply := nats.NewMsg(msg.Reply)
		reply.Data = []byte(`{}`)
		reply.Header.Set(ec2v1.HandledHeader, "1")
		_ = msg.RespondMsg(reply)
	})
	require.NoError(t, err)
	var queue atomic.Int32
	_, err = nc.Subscribe("ec2.ModifyInstanceAttribute", func(msg *nats.Msg) {
		queue.Add(1)
		_ = msg.Respond([]byte(`{}`))
	})
	require.NoError(t, err)

	for _, v := range []bool{true, false} {
		_, err := ModifyInstanceAttribute(context.Background(), &ec2.ModifyInstanceAttributeInput{
			InstanceId:            aws.String(id),
			DisableApiTermination: &ec2.AttributeBooleanValue{Value: aws.Bool(v)},
		}, nc, "acct")
		require.NoError(t, err)
	}
	mu.Lock()
	assert.Equal(t, []bool{true, false}, gotValues, "explicit true and false both reach the owner")
	mu.Unlock()
	assert.Zero(t, queue.Load())

	// A nil value is not a value: queue path, no owner command.
	_, err = ModifyInstanceAttribute(context.Background(), &ec2.ModifyInstanceAttributeInput{
		InstanceId:            aws.String(id),
		DisableApiTermination: &ec2.AttributeBooleanValue{},
	}, nc, "acct")
	require.NoError(t, err)
	assert.Equal(t, int32(1), queue.Load())
	mu.Lock()
	assert.Len(t, gotValues, 2, "no owner command for a nil value")
	mu.Unlock()
}
