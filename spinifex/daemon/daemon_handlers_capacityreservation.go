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
		MemGBPerInstance:      float64(d.resourceMgr.instanceMemChargeMiB(it)) / 1024.0,
		InstanceMatchCriteria: matchCriteria,
		Tenancy:               tenancy,
		InstancePlatform:      aws.StringValue(input.InstancePlatform),
		CreateDate:            time.Now().UTC(),
	}

	if err := d.resourceMgr.CreateReservation(rec); err != nil {
		respondWithError(msg, awserrors.ValidErrorCode(err.Error()))
		return
	}

	// Subscribe the per-reservation launch subject before responding: the SUB and
	// this reply share d.natsConn, so the server registers the responder before
	// the gateway sees the new id and can route a targeted launch to it. A failed
	// subscribe would leave the reservation holding capacity no targeted launch
	// could ever reach, so roll it back rather than return a dead id.
	if err := d.subscribeReservationLaunch(rec.ID); err != nil {
		slog.Error("Failed to subscribe reservation launch subject, rolling back reservation",
			"crId", rec.ID, "err", err)
		d.resourceMgr.CancelReservation(rec.ID, accountID)
		respondWithError(msg, awserrors.ErrorServerInternal)
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
	if found {
		// Drop the launch subject so a targeted launch against the cancelled id
		// hits no responder → InvalidCapacityReservationId.NotFound at the gateway.
		d.unsubscribeReservationLaunch(aws.StringValue(input.CapacityReservationId))
	}
	respondWithJSON(msg, &ec2.CancelCapacityReservationOutput{Return: aws.Bool(found)})
}

// reservationLaunchSubject is the per-reservation subject the owning daemon
// subscribes so the gateway routes a targeted launch straight to it, with no
// cr→node lookup. ErrNoResponders on this subject therefore means the
// reservation is gone (cancelled or lost to a restart).
func reservationLaunchSubject(crID string) string {
	return "ec2.RunInstances.cr." + crID
}

// subscribeReservationLaunch wires the cr launch subject to the standard
// RunInstances handler — the target id rides in the input, so handleEC2RunInstances
// serves it unchanged. Tracked in natsSubscriptions (keyed by cr id) so cancel
// and shutdown can drop it. Idempotent; returns the subscribe error so the
// caller can roll the reservation back rather than leave it unlaunchable.
func (d *Daemon) subscribeReservationLaunch(crID string) error {
	if d.natsConn == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, exists := d.natsSubscriptions[crID]; exists {
		return nil
	}
	sub, err := d.natsConn.Subscribe(reservationLaunchSubject(crID), d.handleEC2RunInstances)
	if err != nil {
		return err
	}
	d.natsSubscriptions[crID] = sub
	return nil
}

// unsubscribeReservationLaunch drops the cr launch subscription on cancel.
func (d *Daemon) unsubscribeReservationLaunch(crID string) {
	d.mu.Lock()
	sub, ok := d.natsSubscriptions[crID]
	if ok {
		delete(d.natsSubscriptions, crID)
	}
	d.mu.Unlock()
	if ok {
		if err := sub.Unsubscribe(); err != nil {
			slog.Warn("Failed to unsubscribe reservation launch subject", "crId", crID, "err", err)
		}
	}
}

// toAWSCapacityReservation renders the in-memory record as the AWS API type.
// Reservations are always active with AvailableInstanceCount = Total − Consumed,
// and never expire (EndDateType=unlimited).
func (r *capacityReservation) toAWSCapacityReservation() *ec2.CapacityReservation {
	return &ec2.CapacityReservation{
		CapacityReservationId:  aws.String(r.ID),
		OwnerId:                aws.String(r.AccountID),
		InstanceType:           aws.String(r.InstanceType),
		InstancePlatform:       aws.String(r.InstancePlatform),
		AvailabilityZone:       aws.String(r.AvailabilityZone),
		TotalInstanceCount:     aws.Int64(int64(r.TotalInstanceCount)),
		AvailableInstanceCount: aws.Int64(int64(r.TotalInstanceCount - r.ConsumedCount)),
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
