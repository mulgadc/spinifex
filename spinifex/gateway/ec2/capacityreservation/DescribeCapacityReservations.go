package gateway_ec2_capacityreservation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// validCapacityReservationFilters is the set of Describe filter names matched
// against reservation fields. tag: filters are accepted by ParseFilters but never
// match, since reservations carry no tags.
var validCapacityReservationFilters = map[string]bool{
	"instance-type":           true,
	"availability-zone":       true,
	"state":                   true,
	"tenancy":                 true,
	"instance-platform":       true,
	"instance-match-criteria": true,
	"owner-id":                true,
}

// DescribeCapacityReservations fans out to every node, aggregates each daemon's
// in-memory reservations, and applies the requested ids and filters. Account
// scoping is enforced by the daemons (which key ListReservations on the caller's
// account id from the request header).
func DescribeCapacityReservations(ctx context.Context, input *ec2.DescribeCapacityReservationsInput, natsConn *nats.Conn, expectedNodes int, accountID string) (ec2.DescribeCapacityReservationsOutput, error) {
	var output ec2.DescribeCapacityReservationsOutput
	if input == nil {
		input = &ec2.DescribeCapacityReservationsInput{}
	}

	filters, err := filterutil.ParseFilters(input.Filters, validCapacityReservationFilters)
	if err != nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	payload, err := json.Marshal(input)
	if err != nil {
		return output, fmt.Errorf("failed to marshal input: %w", err)
	}

	frames, _, err := utils.GatherCtx(ctx, natsConn, "ec2.DescribeCapacityReservations", payload,
		utils.GatherOpts{Timeout: censusTimeout, ExpectedNodes: expectedNodes, AccountID: accountID})
	if err != nil {
		return output, err
	}

	var all []*ec2.CapacityReservation
	for _, frame := range frames {
		var o ec2.DescribeCapacityReservationsOutput
		if json.Unmarshal(frame, &o) == nil {
			all = append(all, o.CapacityReservations...)
		}
	}

	output.CapacityReservations = filterReservations(all, aws.StringValueSlice(input.CapacityReservationIds), filters)
	return output, nil
}

// filterReservations keeps the reservations whose id is in ids (when non-empty)
// and that satisfy every parsed filter.
func filterReservations(reservations []*ec2.CapacityReservation, ids []string, filters map[string][]string) []*ec2.CapacityReservation {
	idSet := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		idSet[id] = struct{}{}
	}

	var out []*ec2.CapacityReservation
	for _, r := range reservations {
		if r == nil {
			continue
		}
		if len(idSet) > 0 {
			if _, ok := idSet[aws.StringValue(r.CapacityReservationId)]; !ok {
				continue
			}
		}
		if !reservationMatchesFilters(r, filters) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// reservationMatchesFilters reports whether r satisfies every filter. Each filter's
// values are OR'd (with wildcard support); filters are AND'd together. A tag: or
// otherwise unhandled filter never matches an untagged reservation.
func reservationMatchesFilters(r *ec2.CapacityReservation, filters map[string][]string) bool {
	for name, values := range filters {
		var field string
		switch name {
		case "instance-type":
			field = aws.StringValue(r.InstanceType)
		case "availability-zone":
			field = aws.StringValue(r.AvailabilityZone)
		case "state":
			field = aws.StringValue(r.State)
		case "tenancy":
			field = aws.StringValue(r.Tenancy)
		case "instance-platform":
			field = aws.StringValue(r.InstancePlatform)
		case "instance-match-criteria":
			field = aws.StringValue(r.InstanceMatchCriteria)
		case "owner-id":
			field = aws.StringValue(r.OwnerId)
		default:
			return false
		}
		if !filterutil.MatchesAny(values, field) {
			return false
		}
	}
	return true
}
