package gateway_ec2_capacityreservation

import (
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// createReservationTimeout bounds the node-targeted Create request-reply. The
// pinned daemon only re-checks fit and mutates in-memory state, so it is fast.
const createReservationTimeout = 30 * time.Second

// ValidateCreateCapacityReservationInput enforces the parameter rules that need no
// cluster knowledge. Type and AZ existence are validated against the census in
// CreateCapacityReservation. Cosmetic fields (InstancePlatform, InstanceMatchCriteria)
// are accepted and stored unchanged.
func ValidateCreateCapacityReservationInput(input *ec2.CreateCapacityReservationInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if aws.StringValue(input.InstanceType) == "" {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if aws.Int64Value(input.InstanceCount) <= 0 {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if aws.StringValue(input.AvailabilityZone) == "" {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	// No AZ-id mapping exists; only AvailabilityZone is honoured.
	if aws.StringValue(input.AvailabilityZoneId) != "" {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	// Reservations never auto-expire: reject any limited end date.
	if input.EndDate != nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if t := aws.StringValue(input.EndDateType); t != "" && t != ec2.EndDateTypeUnlimited {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if t := aws.StringValue(input.Tenancy); t != "" && t != ec2.CapacityReservationTenancyDefault {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return nil
}

// CreateCapacityReservation runs a census-complete fan-out, pins the highest-
// available in-AZ node that fits the whole InstanceCount, and node-targets Create.
// No eligible node (or a lost fit re-check) yields InsufficientInstanceCapacity.
func CreateCapacityReservation(input *ec2.CreateCapacityReservationInput, natsConn *nats.Conn, expectedNodes int, accountID string) (ec2.CreateCapacityReservationOutput, error) {
	var output ec2.CreateCapacityReservationOutput

	if err := ValidateCreateCapacityReservationInput(input); err != nil {
		return output, err
	}
	if aws.BoolValue(input.DryRun) {
		return output, errors.New(awserrors.ErrorDryRunOperation)
	}

	census, err := collectCensus(natsConn, expectedNodes, accountID)
	if err != nil {
		return output, err
	}
	if len(census) == 0 {
		return output, errors.New(awserrors.ErrorInsufficientInstanceCapacity)
	}

	az := aws.StringValue(input.AvailabilityZone)
	if !azInCensus(census, az) {
		return output, errors.New(awserrors.ErrorInvalidAvailabilityZone)
	}

	instanceType := aws.StringValue(input.InstanceType)
	if !typeInCensus(census, instanceType) {
		return output, errors.New(awserrors.ErrorInvalidInstanceType)
	}

	node := selectNode(census, az, instanceType, int(aws.Int64Value(input.InstanceCount)))
	if node == "" {
		return output, errors.New(awserrors.ErrorInsufficientInstanceCapacity)
	}

	subject := fmt.Sprintf("ec2.CreateCapacityReservation.%s", node)
	reservation, err := utils.NATSRequest[ec2.CapacityReservation](natsConn, subject, input, createReservationTimeout, accountID)
	if err != nil {
		return output, err
	}

	output.CapacityReservation = reservation
	return output, nil
}
