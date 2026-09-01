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

// shrinkPasswordDataTimeout replaces the NATS request timeout with a small
// value for the duration of the test, so the ErrTimeout path doesn't burn a
// real 5 seconds.
func shrinkPasswordDataTimeout(t *testing.T) {
	t.Helper()
	prev := getPasswordDataTimeout
	getPasswordDataTimeout = 50 * time.Millisecond
	t.Cleanup(func() { getPasswordDataTimeout = prev })
}

func TestValidateGetPasswordDataInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   *ec2.GetPasswordDataInput
		wantErr string
	}{
		{
			name:    "nil input",
			input:   nil,
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name:    "nil InstanceId",
			input:   &ec2.GetPasswordDataInput{},
			wantErr: awserrors.ErrorMissingParameter,
		},
		{
			name:    "empty string InstanceId",
			input:   &ec2.GetPasswordDataInput{InstanceId: aws.String("")},
			wantErr: awserrors.ErrorMissingParameter,
		},
		{
			name:  "valid input",
			input: &ec2.GetPasswordDataInput{InstanceId: aws.String("i-0123456789abcdef0")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateGetPasswordDataInput(tt.input)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, tt.wantErr, err.Error())
		})
	}
}

func TestGetPasswordData_Success(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	instanceID := "i-0123456789abcdef0"
	wantPasswordData := "ZW5jcnlwdGVkLXBheWxvYWQ="

	var gotSubject string
	var gotAccountID string
	sub, err := nc.Subscribe("ec2."+instanceID+".GetPasswordData", func(msg *nats.Msg) {
		gotSubject = msg.Subject
		gotAccountID = msg.Header.Get(utils.AccountIDHeader)

		var receivedInput ec2.GetPasswordDataInput
		require.NoError(t, json.Unmarshal(msg.Data, &receivedInput))
		assert.Equal(t, instanceID, *receivedInput.InstanceId)

		output := ec2.GetPasswordDataOutput{
			InstanceId:   aws.String(instanceID),
			PasswordData: aws.String(wantPasswordData),
			Timestamp:    aws.Time(time.Unix(1700000000, 0)),
		}
		data, marshalErr := json.Marshal(output)
		require.NoError(t, marshalErr)
		require.NoError(t, msg.Respond(data))
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.GetPasswordDataInput{InstanceId: aws.String(instanceID)}
	output, err := GetPasswordData(context.Background(), input, nc, "123456789012")

	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Equal(t, instanceID, *output.InstanceId)
	require.NotNil(t, output.PasswordData)
	assert.Equal(t, wantPasswordData, *output.PasswordData)
	assert.NotNil(t, output.Timestamp)

	assert.Equal(t, "ec2."+instanceID+".GetPasswordData", gotSubject)
	assert.Equal(t, "123456789012", gotAccountID)
}

func TestGetPasswordData_NoResponders(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	// No subscriber on the topic: an unknown/stopped/terminated instance.
	input := &ec2.GetPasswordDataInput{InstanceId: aws.String("i-nosubscriber")}
	output, err := GetPasswordData(context.Background(), input, nc, "123456789012")

	require.Error(t, err)
	assert.Nil(t, output)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}

func TestGetPasswordData_Timeout(t *testing.T) {
	shrinkPasswordDataTimeout(t)
	_, nc := startTestNATSServer(t)

	instanceID := "i-slow"

	// A live subscriber that never replies forces a real ErrTimeout rather
	// than the immediate ErrNoResponders a missing subscriber would give.
	sub, err := nc.Subscribe("ec2."+instanceID+".GetPasswordData", func(msg *nats.Msg) {})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.GetPasswordDataInput{InstanceId: aws.String(instanceID)}
	output, err := GetPasswordData(context.Background(), input, nc, "123456789012")

	require.Error(t, err)
	assert.Nil(t, output)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}

func TestGetPasswordData_DaemonError(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	instanceID := "i-error"
	sub, err := nc.Subscribe("ec2."+instanceID+".GetPasswordData", func(msg *nats.Msg) {
		require.NoError(t, msg.Respond(utils.GenerateErrorPayload(awserrors.ErrorInvalidInstanceIDNotFound)))
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.GetPasswordDataInput{InstanceId: aws.String(instanceID)}
	output, err := GetPasswordData(context.Background(), input, nc, "123456789012")

	require.Error(t, err)
	assert.Nil(t, output)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}

func TestGetPasswordData_MalformedResponse(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	instanceID := "i-malformed"
	sub, err := nc.Subscribe("ec2."+instanceID+".GetPasswordData", func(msg *nats.Msg) {
		require.NoError(t, msg.Respond([]byte("not json")))
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	input := &ec2.GetPasswordDataInput{InstanceId: aws.String(instanceID)}
	output, err := GetPasswordData(context.Background(), input, nc, "123456789012")

	require.Error(t, err)
	assert.Nil(t, output)
	assert.Contains(t, err.Error(), "failed to unmarshal response")
}
