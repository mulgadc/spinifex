package gateway_ec2_instance

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A nil, empty, or non-"i-" instance ID is rejected at the gateway before any
// NATS round-trip, so a nil connection is never dereferenced.
func TestModifyInstanceMetadataOptions_RejectsMalformedID(t *testing.T) {
	cases := map[string]*ec2.ModifyInstanceMetadataOptionsInput{
		"nil input":  nil,
		"nil id":     {},
		"empty id":   {InstanceId: aws.String("")},
		"bad prefix": {InstanceId: aws.String("x-12345")},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ModifyInstanceMetadataOptions(in, nil, "acct")
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorInvalidInstanceIDMalformed, err.Error())
		})
	}
}
