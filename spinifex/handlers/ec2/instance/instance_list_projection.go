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

// ProjectInstanceList turns a DescribeInstances-shaped request and an
// already-collected instance list into grouped reservations. It is the
// KV/cache counterpart of DescribeInstances' vmMgr.View loop — same
// visibility rule (IsInstanceVisibleToCaller), same per-instance projection
// via ProjectInstance, same filter application via instanceMatchesFilters —
// applied to a []*vm.VM instead of the in-memory running map, so a caller
// outside this package (a gateway-side cache) can call it directly with no
// InstanceServiceImpl receiver and no s.config.AZ read.
//
// fallbackCode/fallbackName label an instance whose stored status has no EC2
// mapping. Runtime network and the capacity reservation are excluded
// (IncludeRuntimeNetwork stays false): both are released on stop, so neither
// a stopped nor a terminated instance has them to project.
func ProjectInstanceList(ctx context.Context, input *ec2.DescribeInstancesInput, accountID string, instances []*vm.VM, az string, fallbackCode int64, fallbackName, opName string) (*ec2.DescribeInstancesOutput, error) {
	instanceIDFilter, err := ParseInstanceIDFilter(input.InstanceIds)
	if err != nil {
		return nil, err
	}

	parsedFilters, err := filterutil.ParseFilters(input.Filters, DescribeInstancesValidFilters)
	if err != nil {
		slog.WarnContext(ctx, opName+": invalid filter", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	reservationMap := make(map[string]*ec2.Reservation)

	for _, instance := range instances {
		if !IsInstanceVisibleToCaller(accountID, instance) {
			continue
		}
		if len(instanceIDFilter) > 0 && !instanceIDFilter[instance.ID] {
			continue
		}
		if instance.Reservation == nil || instance.Instance == nil {
			slog.WarnContext(ctx, opName+": skipping instance with nil Reservation/Instance (data integrity issue)",
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

		if len(parsedFilters) > 0 && !instanceMatchesFilters(instance, projected, parsedFilters) {
			continue
		}

		reservationMap[resID].Instances = append(reservationMap[resID].Instances, projected)
	}

	reservations := make([]*ec2.Reservation, 0, len(reservationMap))
	for _, reservation := range reservationMap {
		reservations = append(reservations, reservation)
	}

	slog.InfoContext(ctx, opName+" completed", "count", len(reservations))
	return &ec2.DescribeInstancesOutput{Reservations: reservations}, nil
}
