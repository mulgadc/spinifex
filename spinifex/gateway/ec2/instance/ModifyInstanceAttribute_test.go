package gateway_ec2_instance

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Validation tests ---

func TestValidateModifyInstanceAttributeInput_NilInput(t *testing.T) {
	err := ValidateModifyInstanceAttributeInput(nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

func TestValidateModifyInstanceAttributeInput_MissingInstanceId(t *testing.T) {
	err := ValidateModifyInstanceAttributeInput(&ec2.ModifyInstanceAttributeInput{})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDMalformed, err.Error())
}

func TestValidateModifyInstanceAttributeInput_EmptyInstanceId(t *testing.T) {
	err := ValidateModifyInstanceAttributeInput(&ec2.ModifyInstanceAttributeInput{
		InstanceId: aws.String(""),
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDMalformed, err.Error())
}

func TestValidateModifyInstanceAttributeInput_BadPrefix(t *testing.T) {
	err := ValidateModifyInstanceAttributeInput(&ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String("x-12345"),
		InstanceType: &ec2.AttributeValue{Value: aws.String("t3.micro")},
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDMalformed, err.Error())
}

func TestValidateModifyInstanceAttributeInput_NoAttributeSet(t *testing.T) {
	err := ValidateModifyInstanceAttributeInput(&ec2.ModifyInstanceAttributeInput{
		InstanceId: aws.String("i-abc123"),
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

func TestValidateModifyInstanceAttributeInput_MultipleAttributes(t *testing.T) {
	err := ValidateModifyInstanceAttributeInput(&ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String("i-abc123"),
		InstanceType: &ec2.AttributeValue{Value: aws.String("t3.micro")},
		UserData:     &ec2.BlobAttributeValue{Value: []byte("data")},
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

func TestValidateModifyInstanceAttributeInput_EmptyInstanceType(t *testing.T) {
	err := ValidateModifyInstanceAttributeInput(&ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String("i-abc123"),
		InstanceType: &ec2.AttributeValue{Value: aws.String("")},
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceAttributeValue, err.Error())
}

func TestValidateModifyInstanceAttributeInput_NilInstanceTypeValue(t *testing.T) {
	err := ValidateModifyInstanceAttributeInput(&ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String("i-abc123"),
		InstanceType: &ec2.AttributeValue{},
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceAttributeValue, err.Error())
}

func TestValidateModifyInstanceAttributeInput_ValidInstanceType(t *testing.T) {
	err := ValidateModifyInstanceAttributeInput(&ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String("i-abc123"),
		InstanceType: &ec2.AttributeValue{Value: aws.String("t3.medium")},
	})
	assert.NoError(t, err)
}

func TestValidateModifyInstanceAttributeInput_ValidUserData(t *testing.T) {
	err := ValidateModifyInstanceAttributeInput(&ec2.ModifyInstanceAttributeInput{
		InstanceId: aws.String("i-abc123"),
		UserData:   &ec2.BlobAttributeValue{Value: []byte("IyEvYmluL2Jhc2g=")},
	})
	assert.NoError(t, err)
}

func TestValidateModifyInstanceAttributeInput_ValidDisableApiTermination(t *testing.T) {
	err := ValidateModifyInstanceAttributeInput(&ec2.ModifyInstanceAttributeInput{
		InstanceId:            aws.String("i-abc123"),
		DisableApiTermination: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	})
	assert.NoError(t, err)
}

func TestValidateModifyInstanceAttributeInput_DisableApiTerminationWithOther(t *testing.T) {
	err := ValidateModifyInstanceAttributeInput(&ec2.ModifyInstanceAttributeInput{
		InstanceId:            aws.String("i-abc123"),
		DisableApiTermination: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
		InstanceType:          &ec2.AttributeValue{Value: aws.String("t3.micro")},
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

func TestValidateModifyInstanceAttributeInput_ValidSourceDestCheck(t *testing.T) {
	err := ValidateModifyInstanceAttributeInput(&ec2.ModifyInstanceAttributeInput{
		InstanceId:      aws.String("i-abc123"),
		SourceDestCheck: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	})
	assert.NoError(t, err)
}

func TestValidateModifyInstanceAttributeInput_SourceDestCheckWithOtherAttribute(t *testing.T) {
	err := ValidateModifyInstanceAttributeInput(&ec2.ModifyInstanceAttributeInput{
		InstanceId:      aws.String("i-abc123"),
		SourceDestCheck: &ec2.AttributeBooleanValue{Value: aws.Bool(false)},
		InstanceType:    &ec2.AttributeValue{Value: aws.String("t3.micro")},
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

// --- Gateway function tests ---

func TestModifyInstanceAttribute_Success(t *testing.T) {
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
	_, nc := startTestNATSServer(t)

	_, err := ModifyInstanceAttribute(context.Background(), nil, nc, "123456789012")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}
