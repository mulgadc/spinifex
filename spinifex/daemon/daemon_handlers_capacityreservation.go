package daemon

import (
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/instancetypes"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// handleEC2CreateCapacityReservation is the node-targeted Create handler: resolve
// the per-instance carve-out from the local catalog, generate the id, and commit
// under the fit re-check. A lost race returns InsufficientInstanceCapacity.
func (d *Daemon) handleEC2CreateCapacityReservation(msg *nats.Msg) {
	accountID := utils.AccountIDFromMsg(msg)
	input := new(ec2.CreateCapacityReservationInput)
	if errResp := utils.UnmarshalJsonPayload(input, msg.Data); errResp != nil {
		if err := msg.Respond(errResp); err != nil {
			slog.Error("Failed to respond to NATS request", "err", err)
		}
		return
	}

	// GPU capacity reservations are out of scope; an unknown type is equally
	// unreservable. Both reject as InvalidInstanceType.
	instanceType := aws.StringValue(input.InstanceType)
	it := d.resourceMgr.instanceTypes[instanceType]
	if it == nil || instancetypes.IsGPUType(it) {
		respondWithError(msg, awserrors.ErrorInvalidInstanceType)
		return
	}

	matchCriteria := aws.StringValue(input.InstanceMatchCriteria)
	if matchCriteria == "" {
		matchCriteria = ec2.InstanceMatchCriteriaOpen
	}
	tenancy := aws.StringValue(input.Tenancy)
	if tenancy == "" {
		tenancy = ec2.CapacityReservationTenancyDefault
	}

	rec := &capacityReservation{
		ID:                    utils.GenerateResourceID("cr"),
		AccountID:             accountID,
		InstanceType:          instanceType,
		AvailabilityZone:      aws.StringValue(input.AvailabilityZone),
		TotalInstanceCount:    int(aws.Int64Value(input.InstanceCount)),
		VCPUPerInstance:       int(instanceTypeVCPUs(it)),
		MemGBPerInstance:      float64(instanceTypeMemoryMiB(it)) / 1024.0,
		InstanceMatchCriteria: matchCriteria,
		Tenancy:               tenancy,
		InstancePlatform:      aws.StringValue(input.InstancePlatform),
		CreateDate:            time.Now().UTC(),
	}

	if err := d.resourceMgr.CreateReservation(rec); err != nil {
		respondWithError(msg, awserrors.ValidErrorCode(err.Error()))
		return
	}

	respondWithJSON(msg, rec.toAWSCapacityReservation())
}

// handleEC2DescribeCapacityReservations is the fan-out Describe handler. Each
// node returns its own in-memory reservations for the account (possibly empty);
// the gateway aggregates and applies id/filter scoping.
func (d *Daemon) handleEC2DescribeCapacityReservations(msg *nats.Msg) {
	accountID := utils.AccountIDFromMsg(msg)

	out := &ec2.DescribeCapacityReservationsOutput{}
	for _, rec := range d.resourceMgr.ListReservations(accountID) {
		out.CapacityReservations = append(out.CapacityReservations, rec.toAWSCapacityReservation())
	}
	respondWithJSON(msg, out)
}

// handleEC2CancelCapacityReservation is the broadcast Cancel handler. Only the
// node owning the reservation releases it; every node acks with Return set so
// the gateway can tell "cancelled" from "no node owns this id".
func (d *Daemon) handleEC2CancelCapacityReservation(msg *nats.Msg) {
	accountID := utils.AccountIDFromMsg(msg)
	input := new(ec2.CancelCapacityReservationInput)
	if errResp := utils.UnmarshalJsonPayload(input, msg.Data); errResp != nil {
		if err := msg.Respond(errResp); err != nil {
			slog.Error("Failed to respond to NATS request", "err", err)
		}
		return
	}

	_, found := d.resourceMgr.CancelReservation(aws.StringValue(input.CapacityReservationId), accountID)
	respondWithJSON(msg, &ec2.CancelCapacityReservationOutput{Return: aws.Bool(found)})
}

// toAWSCapacityReservation renders the in-memory record as the AWS API type.
// Reservations are always active with AvailableInstanceCount equal to the total,
// and never expire (EndDateType=unlimited).
func (r *capacityReservation) toAWSCapacityReservation() *ec2.CapacityReservation {
	count := int64(r.TotalInstanceCount)
	return &ec2.CapacityReservation{
		CapacityReservationId:  aws.String(r.ID),
		OwnerId:                aws.String(r.AccountID),
		InstanceType:           aws.String(r.InstanceType),
		InstancePlatform:       aws.String(r.InstancePlatform),
		AvailabilityZone:       aws.String(r.AvailabilityZone),
		TotalInstanceCount:     aws.Int64(count),
		AvailableInstanceCount: aws.Int64(count),
		State:                  aws.String(ec2.CapacityReservationStateActive),
		StartDate:              aws.Time(r.CreateDate),
		CreateDate:             aws.Time(r.CreateDate),
		InstanceMatchCriteria:  aws.String(r.InstanceMatchCriteria),
		Tenancy:                aws.String(r.Tenancy),
		EndDateType:            aws.String(ec2.EndDateTypeUnlimited),
		EbsOptimized:           aws.Bool(false),
		EphemeralStorage:       aws.Bool(false),
		ReservationType:        aws.String(ec2.CapacityReservationTypeDefault),
	}
}
