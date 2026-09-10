package handlers_ec2_instance

import (
	"context"
	"errors"
	"log/slog"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// InstanceListSelection is a parsed DescribeInstances-shaped request against
// an already-collected instance list: which instances the caller asked about
// and the field filters to apply, parsed once per request. It is the KV/cache
// counterpart of status_selection.go's StatusSelection.
type InstanceListSelection struct {
	accountID   string
	instanceIDs map[string]bool
	filters     map[string][]string
	opName      string
}

// ParseInstanceListSelection validates input and resolves the selection. Its
// errors are deterministic client errors (malformed ID, unknown filter), so a
// caller must parse before fetching the instance list: a request that can
// never succeed must not depend on the list source being reachable to fail.
func ParseInstanceListSelection(ctx context.Context, input *ec2.DescribeInstancesInput, accountID, opName string) (InstanceListSelection, error) {
	instanceIDs, err := ParseInstanceIDFilter(input.InstanceIds)
	if err != nil {
		return InstanceListSelection{}, err
	}

	filters, err := filterutil.ParseFilters(input.Filters, DescribeInstancesValidFilters)
	if err != nil {
		slog.WarnContext(ctx, opName+": invalid filter", "err", err)
		return InstanceListSelection{}, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	return InstanceListSelection{accountID: accountID, instanceIDs: instanceIDs, filters: filters, opName: opName}, nil
}

// Reservations projects instances into grouped ec2.Reservations: same
// visibility rule as the running path (IsInstanceVisibleToCaller), same
// per-instance projection via ProjectInstance, same filter application via
// instanceMatchesFilters — applied to a []*vm.VM instead of the in-memory
// running map, so a caller outside this package (a gateway-side cache) can
// call it directly with no InstanceServiceImpl receiver and no s.config.AZ
// read.
//
// fallbackCode/fallbackName label an instance whose stored status has no EC2
// mapping. Runtime network and the capacity reservation are excluded
// (IncludeRuntimeNetwork stays false): both are released on stop, so neither
// a stopped nor a terminated instance has them to project.
func (sel InstanceListSelection) Reservations(ctx context.Context, instances []*vm.VM, az string, fallbackCode int64, fallbackName string) *ec2.DescribeInstancesOutput {
	reservationMap := make(map[string]*ec2.Reservation)

	for _, instance := range instances {
		if !IsInstanceVisibleToCaller(sel.accountID, instance) {
			continue
		}
		if len(sel.instanceIDs) > 0 && !sel.instanceIDs[instance.ID] {
			continue
		}
		if instance.Reservation == nil || instance.Instance == nil {
			slog.WarnContext(ctx, sel.opName+": skipping instance with nil Reservation/Instance (data integrity issue)",
				"instanceId", instance.ID)
			continue
		}

		resID := ""
		if instance.Reservation.ReservationId != nil {
			resID = *instance.Reservation.ReservationId
		}

		if _, exists := reservationMap[resID]; !exists {
			reservation := &ec2.Reservation{}
			reservation.SetReservationId(resID)
			if instance.Reservation.OwnerId != nil {
				reservation.SetOwnerId(*instance.Reservation.OwnerId)
			}
			reservation.Instances = []*ec2.Instance{}
			reservationMap[resID] = reservation
		}

		projected, _ := ProjectInstance(instance, InstanceProjection{
			AZ:                az,
			FallbackStateCode: fallbackCode,
			FallbackStateName: fallbackName,
		})

		if len(sel.filters) > 0 && !instanceMatchesFilters(instance, projected, sel.filters) {
			continue
		}

		reservationMap[resID].Instances = append(reservationMap[resID].Instances, projected)
	}

	reservations := make([]*ec2.Reservation, 0, len(reservationMap))
	for _, reservation := range reservationMap {
		reservations = append(reservations, reservation)
	}

	slog.InfoContext(ctx, sel.opName+" completed", "count", len(reservations))
	return &ec2.DescribeInstancesOutput{Reservations: reservations}
}
