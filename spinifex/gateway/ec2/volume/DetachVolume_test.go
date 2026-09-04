package gateway_ec2_volume

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateDetachVolumeInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.DetachVolumeInput
		wantErr bool
		errMsg  string
	}{
		{
			name:    "NilInput",
			input:   nil,
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name:    "EmptyInput",
			input:   &ec2.DetachVolumeInput{},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "MissingVolumeId",
			input: &ec2.DetachVolumeInput{
				InstanceId: aws.String("i-1234567890abcdef0"),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "EmptyVolumeId",
			input: &ec2.DetachVolumeInput{
				VolumeId: aws.String(""),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "ValidInput_VolumeOnly",
			input: &ec2.DetachVolumeInput{
				VolumeId: aws.String("vol-1234567890abcdef0"),
			},
			wantErr: false,
		},
		{
			name: "ValidInput_WithInstance",
			input: &ec2.DetachVolumeInput{
				VolumeId:   aws.String("vol-1234567890abcdef0"),
				InstanceId: aws.String("i-1234567890abcdef0"),
			},
			wantErr: false,
		},
		{
			name: "ValidInput_WithForce",
			input: &ec2.DetachVolumeInput{
				VolumeId: aws.String("vol-1234567890abcdef0"),
				Force:    aws.Bool(true),
			},
			wantErr: false,
		},
		{
			name: "ValidInput_AllFields",
			input: &ec2.DetachVolumeInput{
				VolumeId:   aws.String("vol-1234567890abcdef0"),
				InstanceId: aws.String("i-1234567890abcdef0"),
				Device:     aws.String("/dev/sdf"),
				Force:      aws.Bool(true),
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDetachVolumeInput(tt.input)

			if tt.wantErr {
				assert.Error(t, err)
				assert.Equal(t, tt.errMsg, err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestDetachVolume_ForceFallsBackWhenTheInstanceIsGone covers the deadlock the
// control-plane-only detach exists for. A teardown that fails part-way leaves a
// volume attached to an instance that no longer runs anywhere, and the ordinary
// detach routes to that instance's own subject, so it can never be answered.
// Without the fallback the volume is attached forever and therefore undeletable.
func TestDetachVolume_ForceFallsBackWhenTheInstanceIsGone(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	var forced []byte
	sub, err := nc.Subscribe(forceDetachSubject, func(msg *nats.Msg) {
		forced = msg.Data
		data, err := json.Marshal(&ec2.VolumeAttachment{
			VolumeId: aws.String("vol-orphan"), State: aws.String("detached"),
		})
		require.NoError(t, err)
		require.NoError(t, msg.Respond(data))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, nc.Flush())

	// No subscriber on ec2.cmd.i-gone, which is exactly a terminated instance.
	out, err := DetachVolume(context.Background(), &ec2.DetachVolumeInput{
		VolumeId:   aws.String("vol-orphan"),
		InstanceId: aws.String("i-gone"),
		Force:      aws.Bool(true),
	}, nc, "000000000001")
	require.NoError(t, err, "force must release a volume whose instance has no responder")

	assert.Equal(t, "detached", aws.StringValue(out.State))
	assert.Contains(t, string(forced), "vol-orphan", "the force detach must name the volume")
}

// TestDetachVolume_WithoutForceStillReportsTheMissingInstance guards the other
// direction: the fallback clears the attachment without touching the guest, so
// a caller who did not ask for force must keep getting the honest answer rather
// than a silent success.
func TestDetachVolume_WithoutForceStillReportsTheMissingInstance(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	sub, err := nc.Subscribe(forceDetachSubject, func(msg *nats.Msg) {
		require.Fail(t, "force detach must not run without Force")
		_ = msg.Respond(nil)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, nc.Flush())

	_, err = DetachVolume(context.Background(), &ec2.DetachVolumeInput{
		VolumeId:   aws.String("vol-orphan"),
		InstanceId: aws.String("i-gone"),
	}, nc, "000000000001")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}
