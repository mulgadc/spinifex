package instance

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
)

// accountInstanceRecordsSubject serves every non-terminated instance record an
// account owns from the shared store, whichever node holds the instance.
const accountInstanceRecordsSubject = "ec2.DescribeAccountInstanceRecords"

// DescribeAccountInstancesStrict lists every non-terminated instance the
// account owns, or returns a retryable ServiceUnavailable. Existence comes from
// the shared record store, so an instance no answering node holds is still
// listed with its recorded state; node answers are joined on for live state.
// Anything that could make the list shorter than the truth is an error: a
// configured node that did not answer, a frame from a node outside nodeIDs, a
// record the owner could not project, or a store that did not answer.
func DescribeAccountInstancesStrict(ctx context.Context, natsConn *nats.Conn, nodeIDs []string, accountID string, opts ...DescribeOption) (*ec2.DescribeInstancesOutput, error) {
	if len(nodeIDs) == 0 {
		slog.ErrorContext(ctx, "DescribeInstances: strict listing refused, configured node set unknown")
		return nil, incompleteListingError()
	}
	cfg := describeConfig{fanoutTimeout: defaultDescribeFanoutTimeout}
	for _, opt := range opts {
		opt(&cfg)
	}

	jsonData, err := json.Marshal(&ec2.DescribeInstancesInput{})
	if err != nil {
		return nil, awserrors.Errorf(awserrors.ErrorServerInternal, "marshal describe input: %v", err)
	}

	records, ok := queryInstanceBucket(ctx, natsConn, accountInstanceRecordsSubject, jsonData, accountID)
	if !ok {
		slog.WarnContext(ctx, "DescribeInstances: strict listing refused, instance record store did not answer")
		return nil, incompleteListingError()
	}

	// Collect past the point where every configured node has answered, so a
	// frame from a node outside the set is seen rather than raced against.
	frames, sum, err := natsmsg.Gather(ctx, natsConn, "ec2.DescribeInstances", jsonData, natsmsg.GatherOpts{
		Timeout:            cfg.fanoutTimeout,
		AccountID:          accountID,
		ExpectedResponders: len(nodeIDs),
		Mode:               natsmsg.CollectUntilDeadline,
		ResponderGrace:     describeResponderGrace,
	})
	if err != nil {
		return nil, err
	}
	live, complete := judgeIdentityCompleteness(ctx, frames, sum, nodeIDs)
	if stray := unconfiguredNodes(frames, nodeIDs); len(stray) > 0 {
		slog.WarnContext(ctx, "DescribeInstances: strict listing refused, frame from a node outside the configured set",
			"unconfigured_nodes", stray)
		return nil, incompleteListingError()
	}
	if !complete {
		slog.WarnContext(ctx, "DescribeInstances: strict listing refused, fan-out incomplete", "configured_nodes", len(nodeIDs))
		return nil, incompleteListingError()
	}

	return &ec2.DescribeInstancesOutput{Reservations: joinLiveOntoRecords(records, live)}, nil
}

// unconfiguredNodes returns the node IDs that answered but are not in nodeIDs.
func unconfiguredNodes(frames []natsmsg.Frame, nodeIDs []string) []string {
	var stray []string
	for _, frame := range frames {
		if frame.NodeID != "" && !slices.Contains(nodeIDs, frame.NodeID) && !slices.Contains(stray, frame.NodeID) {
			stray = append(stray, frame.NodeID)
		}
	}
	return stray
}

// joinLiveOntoRecords starts from the recorded instances and replaces each with
// the answering node's view of it. An instance only a node reports is added, so
// the result is the union and never the intersection.
func joinLiveOntoRecords(records, live []*ec2.Reservation) []*ec2.Reservation {
	type entry struct {
		reservation *ec2.Reservation
		instance    *ec2.Instance
	}
	byID := map[string]*entry{}
	var order []string
	add := func(reservations []*ec2.Reservation) {
		for _, res := range reservations {
			if res == nil {
				continue
			}
			for _, inst := range res.Instances {
				if inst == nil || inst.InstanceId == nil {
					continue
				}
				id := aws.StringValue(inst.InstanceId)
				if existing, ok := byID[id]; ok {
					existing.instance = inst
					continue
				}
				byID[id] = &entry{reservation: res, instance: inst}
				order = append(order, id)
			}
		}
	}
	add(records)
	add(live)

	out := make([]*ec2.Reservation, 0, len(order))
	for _, id := range order {
		e := byID[id]
		out = append(out, &ec2.Reservation{
			ReservationId: e.reservation.ReservationId,
			OwnerId:       e.reservation.OwnerId,
			Groups:        e.reservation.Groups,
			Instances:     []*ec2.Instance{e.instance},
		})
	}
	return mergeReservationsByID(out)
}
