//go:build integration

package integration

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
)

// TestRunInstances_MalformedAMIIDRejected asserts that an ImageId lacking the
// "ami-" prefix is rejected by ValidateRunInstancesInput
// (gateway/ec2/instance/RunInstances.go) before any NATS hop. No StubSubject
// is registered for spinifex.node.status or any per-node launch subject: if
// the gateway ever dispatched this request instead of rejecting it up front,
// the call would hang on utils.Gather's timeout rather than fail fast, making
// this test itself a tripwire for that regression.
func TestRunInstances_MalformedAMIIDRejected(t *testing.T) {
	gw := StartGateway(t)

	_, err := gw.EC2Client(t).RunInstances(&ec2.RunInstancesInput{
		ImageId:      aws.String("not-an-ami-id"),
		InstanceType: aws.String("t3.micro"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	})
	requireAWSErrorCode(t, err, "InvalidAMIID.Malformed")
}

// TestRunInstances_MalformedCapacityReservationIDRejected asserts that a
// targeted-launch CapacityReservationId lacking the "cr-" prefix is rejected
// by runInstancesInner's gateway-side-only check (RunInstances.go) before the
// launch reaches the capacity-reservation NATS subject. As above, no daemon
// stub is registered, so a regression that let this request through would
// hang instead of failing fast.
func TestRunInstances_MalformedCapacityReservationIDRejected(t *testing.T) {
	gw := StartGateway(t)

	_, err := gw.EC2Client(t).RunInstances(&ec2.RunInstancesInput{
		ImageId:      aws.String("ami-0123456789abcdef0"),
		InstanceType: aws.String("t3.micro"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
		CapacityReservationSpecification: &ec2.CapacityReservationSpecification{
			CapacityReservationTarget: &ec2.CapacityReservationTarget{
				CapacityReservationId: aws.String("not-a-cr-id"),
			},
		},
	})
	requireAWSErrorCode(t, err, "InvalidCapacityReservationId.Malformed")
}
