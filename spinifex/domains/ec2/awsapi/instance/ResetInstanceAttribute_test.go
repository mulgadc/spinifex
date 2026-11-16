package instance

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Bad input is refused before any NATS round-trip, so a nil connection is
// never used. The attribute is checked before the instance ID, as AWS does.
func TestResetInstanceAttribute_RejectsBadInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		attribute string
		id        string
		wantCode  string
		wantMsg   string
	}{
		{name: "missing attribute", id: "i-0f3a9c2b", wantCode: awserrors.ErrorMissingParameter,
			wantMsg: "The request must contain the parameter attribute"},
		{name: "valid instance attribute that cannot be reset", attribute: "instanceType", id: "i-zzz",
			wantCode: awserrors.ErrorInvalidParameterValue, wantMsg: "Value (instanceType) for parameter attribute is invalid. Unknown attribute."},
		{name: "missing instance ID", attribute: "kernel", wantCode: awserrors.ErrorMissingParameter,
			wantMsg: "The request must contain the parameter instanceId"},
		{name: "malformed instance ID", attribute: "sourceDestCheck", id: "i-zzz",
			wantCode: awserrors.ErrorInvalidParameterValue, wantMsg: "Value (i-zzz) for parameter instanceId is invalid. Expected: 'i-...'."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			input := &ec2.ResetInstanceAttributeInput{}
			if tc.attribute != "" {
				input.Attribute = aws.String(tc.attribute)
			}
			if tc.id != "" {
				input.InstanceId = aws.String(tc.id)
			}
			_, err := ResetInstanceAttribute(context.Background(), input, nil, "123456789012")
			code, msg, _ := awserrors.ResolveErrorDetail(err)
			assert.Equal(t, tc.wantCode, code)
			assert.Equal(t, tc.wantMsg, msg)
		})
	}
}

// A well-formed ID, 8 or 17 hex, passes validation and reaches the daemon
// queue group.
func TestResetInstanceAttribute_ForwardsToDaemon(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	_, err := nc.QueueSubscribe("ec2.ResetInstanceAttribute", "spinifex-workers", func(msg *nats.Msg) {
		_ = msg.Respond([]byte(`{}`))
	})
	require.NoError(t, err)

	for _, id := range []string{"i-0f3a9c2b", "i-0f3a9c2b7d1e4a5b6"} {
		out, err := ResetInstanceAttribute(context.Background(), &ec2.ResetInstanceAttributeInput{
			InstanceId: aws.String(id), Attribute: aws.String("ramdisk"),
		}, nc, "123456789012")
		require.NoError(t, err, id)
		require.NotNil(t, out, id)
	}
}
