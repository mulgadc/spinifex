package gateway_ec2_idem_test

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	gateway_ec2_idem "github.com/mulgadc/spinifex/spinifex/gateway/ec2/idem"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every converted action's input type has to yield its token, since the
// decorator reads the field reflectively rather than per action.
func TestTokenAndParams_ReadsEveryConvertedInput(t *testing.T) {
	t.Parallel()
	inputs := map[string]any{
		"CreateVolume":                    &ec2.CreateVolumeInput{ClientToken: aws.String("tok")},
		"CreateNetworkInterface":          &ec2.CreateNetworkInterfaceInput{ClientToken: aws.String("tok")},
		"CreateNatGateway":                &ec2.CreateNatGatewayInput{ClientToken: aws.String("tok")},
		"CreateRouteTable":                &ec2.CreateRouteTableInput{ClientToken: aws.String("tok")},
		"CreateEgressOnlyInternetGateway": &ec2.CreateEgressOnlyInternetGatewayInput{ClientToken: aws.String("tok")},
		"CopyImage":                       &ec2.CopyImageInput{ClientToken: aws.String("tok")},
		"CreateCapacityReservation":       &ec2.CreateCapacityReservationInput{ClientToken: aws.String("tok")},
	}
	for action, input := range inputs {
		token, hash, ok := gateway_ec2_idem.TokenAndParams(input)
		require.True(t, ok, "%s must expose its ClientToken", action)
		assert.Equal(t, "tok", token, action)
		assert.NotEmpty(t, hash, action)
	}
}

// No token means the client did not ask for idempotency, so the request runs
// unwrapped rather than being rejected.
func TestTokenAndParams_NoTokenOptsOut(t *testing.T) {
	t.Parallel()
	cases := map[string]any{
		"nil field":     &ec2.CreateVolumeInput{},
		"empty string":  &ec2.CreateVolumeInput{ClientToken: aws.String("")},
		"nil pointer":   (*ec2.CreateVolumeInput)(nil),
		"no such field": &ec2.DescribeVolumesInput{},
		"not a struct":  aws.String("tok"),
	}
	for name, input := range cases {
		_, _, ok := gateway_ec2_idem.TokenAndParams(input)
		assert.False(t, ok, name)
	}
}

// The hash must ignore the token itself: a retry carries the same token and the
// same params, and a different token with the same params is different work.
func TestTokenAndParams_HashIgnoresTheToken(t *testing.T) {
	t.Parallel()
	first := &ec2.CreateVolumeInput{Size: aws.Int64(8), ClientToken: aws.String("a")}
	sameParams := &ec2.CreateVolumeInput{Size: aws.Int64(8), ClientToken: aws.String("b")}
	changed := &ec2.CreateVolumeInput{Size: aws.Int64(16), ClientToken: aws.String("a")}

	_, firstHash, ok := gateway_ec2_idem.TokenAndParams(first)
	require.True(t, ok)
	_, sameHash, ok := gateway_ec2_idem.TokenAndParams(sameParams)
	require.True(t, ok)
	_, changedHash, ok := gateway_ec2_idem.TokenAndParams(changed)
	require.True(t, ok)

	assert.Equal(t, firstHash, sameHash, "the token must not feed the hash")
	assert.NotEqual(t, firstHash, changedHash, "changed params must change the hash")
}

// The input is dispatched to the handler after hashing, so clearing the token
// for the hash must not clear it on the caller's struct.
func TestTokenAndParams_LeavesTheInputIntact(t *testing.T) {
	t.Parallel()
	input := &ec2.CreateVolumeInput{Size: aws.Int64(8), ClientToken: aws.String("tok")}

	_, _, ok := gateway_ec2_idem.TokenAndParams(input)
	require.True(t, ok)

	require.NotNil(t, input.ClientToken)
	assert.Equal(t, "tok", *input.ClientToken)
}
