package idem_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/internal/testkit"
	ec2idemapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/idem"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/lifecycle/idempotency"
	"github.com/nats-io/nats.go/jetstream"
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
		token, hash, ok := ec2idemapi.TokenAndParams(input)
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
		_, _, ok := ec2idemapi.TokenAndParams(input)
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

	_, firstHash, ok := ec2idemapi.TokenAndParams(first)
	require.True(t, ok)
	_, sameHash, ok := ec2idemapi.TokenAndParams(sameParams)
	require.True(t, ok)
	_, changedHash, ok := ec2idemapi.TokenAndParams(changed)
	require.True(t, ok)

	assert.Equal(t, firstHash, sameHash, "the token must not feed the hash")
	assert.NotEqual(t, firstHash, changedHash, "changed params must change the hash")
}

// The input is dispatched to the handler after hashing, so clearing the token
// for the hash must not clear it on the caller's struct.
func TestTokenAndParams_LeavesTheInputIntact(t *testing.T) {
	t.Parallel()
	input := &ec2.CreateVolumeInput{Size: aws.Int64(8), ClientToken: aws.String("tok")}

	_, _, ok := ec2idemapi.TokenAndParams(input)
	require.True(t, ok)

	require.NotNil(t, input.ClientToken)
	assert.Equal(t, "tok", *input.ClientToken)
}

func runTestBucket(t *testing.T) ec2idemapi.Bucket {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	kv, err := idempotency.OpenBucket(t.Context(), testutil.NewJetStream(t, nc), "ec2-tokens", time.Minute)
	require.NoError(t, err)
	return func(context.Context) (jetstream.KeyValue, error) { return kv, nil }
}

// Reusing a token with different parameters maps to the AWS error and never
// runs the work a second time.
func TestRun_ParamMismatchMapsAWSError(t *testing.T) {
	t.Parallel()
	bucket := runTestBucket(t)
	calls := 0
	work := func() (string, error) { calls++; return "vol-1", nil }

	out, err := ec2idemapi.Run(t.Context(), bucket, "CreateVolume", "acct",
		&ec2.CreateVolumeInput{Size: aws.Int64(8), ClientToken: aws.String("tok")}, work)
	require.NoError(t, err)
	assert.Equal(t, "vol-1", out)

	_, err = ec2idemapi.Run(t.Context(), bucket, "CreateVolume", "acct",
		&ec2.CreateVolumeInput{Size: aws.Int64(9), ClientToken: aws.String("tok")}, work)
	require.EqualError(t, err, awserrors.ErrorIdempotentParameterMismatch)
	assert.Equal(t, 1, calls)
}

// A token the store cannot honour fails the request rather than running it
// unprotected; a request without a token never touches the store.
func TestRun_BucketFailure(t *testing.T) {
	t.Parallel()
	broken := func(context.Context) (jetstream.KeyValue, error) { return nil, errors.New("no jetstream") }
	ran := false
	work := func() (string, error) { ran = true; return "ok", nil }

	_, err := ec2idemapi.Run(t.Context(), broken, "", "acct",
		&ec2.RunInstancesInput{ClientToken: aws.String("tok")}, work)
	require.EqualError(t, err, awserrors.ErrorServerInternal)
	assert.False(t, ran)

	out, err := ec2idemapi.Run(t.Context(), broken, "", "acct", &ec2.RunInstancesInput{}, work)
	require.NoError(t, err)
	assert.Equal(t, "ok", out)
}
