//go:build integration

package integration

import (
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	awscreds "github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubEmptyInstanceBuckets stands in for the DescribeStoppedInstances and
// DescribeTerminatedInstances daemon responders that DescribeInstances also
// queries. Stubbing them keeps the fan-out deterministic and fast regardless
// of the embedded NATS server's no-responders behavior, instead of relying on
// a 3s RequestMsg timeout when nothing answers.
func stubEmptyInstanceBuckets(t *testing.T, gw *Gateway) {
	t.Helper()
	empty := mustMarshal(t, &ec2.DescribeInstancesOutput{})
	gw.StubSubject(t, "ec2.DescribeStoppedInstances", empty)
	gw.StubSubject(t, "ec2.DescribeTerminatedInstances", empty)
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

// TestDescribeInstances_HappyPath proves the harness end to end: the real
// gateway router, real SigV4 auth against a seeded root IAM user, and a
// stubbed ec2.DescribeInstances daemon responder wired together in one
// process with nothing provisioned. A real spinifex daemon would answer
// "ec2.DescribeInstances" with exactly this JSON-encoded ec2.DescribeInstancesOutput.
func TestDescribeInstances_HappyPath(t *testing.T) {
	gw := StartGateway(t)
	stubEmptyInstanceBuckets(t, gw)

	want := &ec2.DescribeInstancesOutput{
		Reservations: []*ec2.Reservation{{
			ReservationId: aws.String("r-abc123"),
			Instances: []*ec2.Instance{{
				InstanceId:   aws.String("i-abc123"),
				InstanceType: aws.String("t2.micro"),
				State:        &ec2.InstanceState{Name: aws.String("running")},
			}},
		}},
	}
	gw.StubSubject(t, "ec2.DescribeInstances", mustMarshal(t, want))

	out, err := gw.EC2Client(t).DescribeInstances(&ec2.DescribeInstancesInput{})
	require.NoError(t, err)
	require.Len(t, out.Reservations, 1)
	require.Len(t, out.Reservations[0].Instances, 1)
	assert.Equal(t, "i-abc123", aws.StringValue(out.Reservations[0].Instances[0].InstanceId))
	assert.Equal(t, "running", aws.StringValue(out.Reservations[0].Instances[0].State.Name))
}

// TestDescribeInstances_ErrorPath drives a stubbed daemon error envelope
// through the same real gateway and asserts the AWS error code the SDK
// surfaces matches what a live environment would return for the same
// daemon-side failure — proving the tier can also cover error-code paths, not
// just happy-path shape assertions.
func TestDescribeInstances_ErrorPath(t *testing.T) {
	gw := StartGateway(t)
	stubEmptyInstanceBuckets(t, gw)

	gw.StubSubject(t, "ec2.DescribeInstances", utils.GenerateErrorPayload(awserrors.ErrorInvalidInstanceIDNotFound))

	_, err := gw.EC2Client(t).DescribeInstances(&ec2.DescribeInstancesInput{})
	require.Error(t, err)

	var awsErr awserr.Error
	require.ErrorAs(t, err, &awsErr, "expected awserr.Error, got %T: %v", err, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, awsErr.Code())
}

// TestDescribeInstances_UnauthenticatedRejected proves the harness exercises
// real SigV4 authentication, not just routing: a request signed with a
// nonexistent access key never reaches the stubbed NATS responder and is
// rejected before dispatch.
func TestDescribeInstances_UnauthenticatedRejected(t *testing.T) {
	gw := StartGateway(t)
	stubEmptyInstanceBuckets(t, gw)
	// Deliberately not stubbed: if auth failed to reject the request first,
	// DescribeInstances would hang waiting on ExpectedNodes=1 that never answers.

	sess := gw.session(t)
	sess.Config.Credentials = awscreds.NewStaticCredentials("AKIADOESNOTEXIST0000", "wrong-secret", "")

	_, err := ec2.New(sess).DescribeInstances(&ec2.DescribeInstancesInput{})
	require.Error(t, err)

	var awsErr awserr.Error
	require.ErrorAs(t, err, &awsErr, "expected awserr.Error, got %T: %v", err, err)
	assert.Equal(t, awserrors.ErrorInvalidClientTokenId, awsErr.Code())
}
