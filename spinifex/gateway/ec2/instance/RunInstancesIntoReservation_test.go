package gateway_ec2_instance

import (
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

const testCRID = "cr-0123456789abcdef0"

// crTargetedInput clones the valid defaults and targets a capacity reservation.
func crTargetedInput(crID string) *ec2.RunInstancesInput {
	in := defaults
	in.CapacityReservationSpecification = &ec2.CapacityReservationSpecification{
		CapacityReservationTarget: &ec2.CapacityReservationTarget{
			CapacityReservationId: aws.String(crID),
		},
	}
	return &in
}

func TestCapacityReservationTargetID(t *testing.T) {
	assert.Empty(t, capacityReservationTargetID(&ec2.RunInstancesInput{}), "no spec → general path")
	assert.Empty(t, capacityReservationTargetID(&ec2.RunInstancesInput{
		CapacityReservationSpecification: &ec2.CapacityReservationSpecification{
			CapacityReservationPreference: aws.String(ec2.CapacityReservationPreferenceOpen),
		},
	}), "open without target → general path")
	assert.Equal(t, testCRID, capacityReservationTargetID(crTargetedInput(testCRID)))
}

// A malformed reservation id is rejected at the gateway before any NATS call.
func TestRunInstancesInner_TargetedMalformedID(t *testing.T) {
	_, err := runInstancesInner(crTargetedInput("bogus-id"), nil, nil, "123456789012", nil, nil, 1)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidCapacityReservationIdMalformed, err.Error())
}

// The placement-group + capacity-reservation combination is rejected (Phase 2).
func TestRunInstancesInner_TargetedWithPlacementGroup(t *testing.T) {
	input := crTargetedInput(testCRID)
	input.Placement = &ec2.Placement{GroupName: aws.String("pg-cluster")}
	_, err := runInstancesInner(input, nil, nil, "123456789012", nil, nil, 1)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

// No responder on the cr-subject (cancelled / restarted reservation) maps the
// transport-level ErrNoResponders to InvalidCapacityReservationId.NotFound.
func TestRunIntoReservation_NoResponder(t *testing.T) {
	_, nc := startTestNATSServer(t)
	_, err := runIntoReservation(crTargetedInput(testCRID), nc, "123456789012", testCRID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidCapacityReservationIdNotFound, err.Error())
}

// A targeted launch routes to ec2.RunInstances.cr.<crID> and returns the owning
// daemon's reservation verbatim.
func TestRunIntoReservation_Success(t *testing.T) {
	_, nc := startTestNATSServer(t)

	sub, err := nc.Subscribe("ec2.RunInstances.cr."+testCRID, func(msg *nats.Msg) {
		data, _ := json.Marshal(ec2.Reservation{
			ReservationId: aws.String("r-cr"),
			Instances:     []*ec2.Instance{{InstanceId: aws.String("i-cr1")}},
		})
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()
	time.Sleep(50 * time.Millisecond)

	reservation, err := runIntoReservation(crTargetedInput(testCRID), nc, "123456789012", testCRID)
	require.NoError(t, err)
	require.Len(t, reservation.Instances, 1)
	assert.Equal(t, "i-cr1", aws.StringValue(reservation.Instances[0].InstanceId))
}

// A semantic error from the owning daemon (e.g. a full reservation) rides back
// as its awserror code, not the generic NotFound.
func TestRunIntoReservation_DaemonErrorPropagates(t *testing.T) {
	_, nc := startTestNATSServer(t)

	sub, err := nc.Subscribe("ec2.RunInstances.cr."+testCRID, func(msg *nats.Msg) {
		_ = msg.Respond(utils.GenerateErrorPayload(awserrors.ErrorReservationCapacityExceeded))
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()
	time.Sleep(50 * time.Millisecond)

	_, err = runIntoReservation(crTargetedInput(testCRID), nc, "123456789012", testCRID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorReservationCapacityExceeded, err.Error())
}

// The full runInstancesInner branch routes a targeted launch to the cr-subject
// (not the general distribute path) and enriches the result.
func TestRunInstancesInner_TargetedRoutesToCRSubject(t *testing.T) {
	_, nc := startTestNATSServer(t)

	sub, err := nc.Subscribe("ec2.RunInstances.cr."+testCRID, func(msg *nats.Msg) {
		data, _ := json.Marshal(ec2.Reservation{
			ReservationId: aws.String("r-cr"),
			Instances:     []*ec2.Instance{{InstanceId: aws.String("i-cr1")}},
		})
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()
	time.Sleep(50 * time.Millisecond)

	reservation, err := runInstancesInner(crTargetedInput(testCRID), nc, nil, "123456789012", nil, nil, 1)
	require.NoError(t, err)
	require.Len(t, reservation.Instances, 1)
	assert.Equal(t, "i-cr1", aws.StringValue(reservation.Instances[0].InstanceId))
}
