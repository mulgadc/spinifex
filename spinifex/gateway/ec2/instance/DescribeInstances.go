package gateway_ec2_instance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// DescribeInstances fans out to all nodes via NATS and aggregates the results.
// It is lenient: partial results from a slow or unreachable node are returned
// without error, which is correct for user-facing describes. Callers that must
// not act on a partial view (the quota reconcile) use DescribeInstancesForReconcile.
func DescribeInstances(ctx context.Context, input *ec2.DescribeInstancesInput, natsConn *nats.Conn, expectedNodes int, accountID string) (*ec2.DescribeInstancesOutput, error) {
	reservations, _, firstClient4xx, err := gatherInstances(ctx, input, natsConn, expectedNodes, accountID)
	if err != nil {
		return nil, err
	}
	// Propagate a deterministic 4xx only when nothing was collected (fan-out + KV).
	if firstClient4xx != "" && len(reservations) == 0 {
		return nil, errors.New(firstClient4xx)
	}
	return &ec2.DescribeInstancesOutput{Reservations: reservations}, nil
}

// DescribeInstancesForReconcile is the strict variant the quota reconcile uses.
// complete is true only when the sweep observed every expected node and both
// instance buckets, so reconcile may lower a counter only from a provably
// complete view. A partial sweep — a node down, a timed-out fan-out, or a failed
// bucket query — returns complete=false, and reconcile leaves the counter for the
// next clean pass rather than under-counting usage and lifting the cap.
func DescribeInstancesForReconcile(ctx context.Context, input *ec2.DescribeInstancesInput, natsConn *nats.Conn, expectedNodes int, accountID string) (reservations []*ec2.Reservation, complete bool, err error) {
	reservations, complete, _, err = gatherInstances(ctx, input, natsConn, expectedNodes, accountID)
	return reservations, complete, err
}

// gatherInstances runs the running-instance fan-out plus the stopped/terminated
// KV bucket queries and aggregates every reservation. complete reports whether
// the sweep saw a success frame from all expectedNodes without timing out and
// both bucket queries succeeded — the precondition reconcile needs before it may
// lower a counter. firstClient4xx carries the first deterministic 4xx for the
// lenient caller to surface when nothing was collected.
func gatherInstances(ctx context.Context, input *ec2.DescribeInstancesInput, natsConn *nats.Conn, expectedNodes int, accountID string) (reservations []*ec2.Reservation, complete bool, firstClient4xx string, err error) {
	jsonData, err := json.Marshal(input)
	if err != nil {
		slog.ErrorContext(ctx, "DescribeInstances: Failed to marshal input", "err", err)
		return nil, false, "", fmt.Errorf("failed to marshal input: %w", err)
	}

	frames, sum, err := utils.GatherCtx(ctx, natsConn, "ec2.DescribeInstances", jsonData,
		utils.GatherOpts{Timeout: 3 * time.Second, ExpectedNodes: expectedNodes, AccountID: accountID})
	if err != nil {
		return nil, false, "", err
	}

	var allReservations []*ec2.Reservation
	for _, frame := range frames {
		var nodeOutput ec2.DescribeInstancesOutput
		if json.Unmarshal(frame, &nodeOutput) == nil && nodeOutput.Reservations != nil {
			allReservations = append(allReservations, nodeOutput.Reservations...)
		}
	}

	// The fan-out is complete only when every expected node answered with a
	// success frame and the deadline was not hit; an error frame or a missing
	// node leaves the view partial.
	fanoutComplete := expectedNodes > 0 && !sum.TimedOut && sum.Successes >= expectedNodes

	// Both topics use queue groups (single responder each); query in parallel.
	var kvMu sync.Mutex
	var kvWg sync.WaitGroup
	bucketsOK := true
	for _, topic := range []string{"ec2.DescribeStoppedInstances", "ec2.DescribeTerminatedInstances"} {
		kvWg.Add(1)
		go func(topic string) {
			defer kvWg.Done()
			reservations, ok := queryInstanceBucket(ctx, natsConn, topic, jsonData, accountID)
			kvMu.Lock()
			defer kvMu.Unlock()
			if !ok {
				bucketsOK = false
			}
			allReservations = append(allReservations, reservations...)
		}(topic)
	}
	kvWg.Wait()

	slog.InfoContext(ctx, "DescribeInstances: Aggregated response", "total_reservations", len(allReservations))
	return allReservations, fanoutComplete && bucketsOK, sum.FirstClient4xx, nil
}

// EnrichInstanceProfileIDs resolves IamInstanceProfile.Id for every instance
// that carries only an ARN. Results are cached per ARN to avoid repeated RPCs.
// Misses are warn-logged and leave Id empty (graceful degradation for deleted profiles).
// Safe to call with a nil output or nil IAMService (no-op).
func EnrichInstanceProfileIDs(out *ec2.DescribeInstancesOutput, iamSvc handlers_iam.IAMService, accountID string) {
	if out == nil || iamSvc == nil {
		return
	}
	cache := map[string]string{} // ARN → ID; "" = miss
	for _, res := range out.Reservations {
		if res == nil {
			continue
		}
		for _, inst := range res.Instances {
			if inst == nil || inst.IamInstanceProfile == nil {
				continue
			}
			arn := aws.StringValue(inst.IamInstanceProfile.Arn)
			if arn == "" {
				continue
			}
			id, cached := cache[arn]
			if !cached {
				profile, err := iamSvc.ResolveInstanceProfile(accountID, arn)
				if err != nil || profile == nil {
					slog.Warn("DescribeInstances: failed to resolve instance profile ID",
						"arn", arn, "err", err)
					cache[arn] = ""
				} else {
					id = profile.InstanceProfileID
					cache[arn] = id
				}
			}
			if id != "" {
				inst.IamInstanceProfile.Id = aws.String(id)
			}
		}
	}
}

// queryInstanceBucket queries a single describe topic and returns its
// reservations. ok is false when the query failed (request error or error
// payload), so a reconcile caller can treat the sweep as incomplete rather than
// silently dropping the bucket's instances.
func queryInstanceBucket(ctx context.Context, natsConn *nats.Conn, topic string, jsonData []byte, accountID string) (reservations []*ec2.Reservation, ok bool) {
	reqMsg := nats.NewMsg(topic)
	reqMsg.Data = jsonData
	reqMsg.Header.Set(utils.AccountIDHeader, accountID)
	utils.InjectTraceContext(ctx, reqMsg.Header)
	msg, err := natsConn.RequestMsg(reqMsg, 3*time.Second)
	if err != nil {
		slog.WarnContext(ctx, "DescribeInstances: Failed to query instance bucket", "topic", topic, "err", err)
		return nil, false
	}
	if responseError, parseErr := utils.ValidateErrorPayload(msg.Data); parseErr != nil {
		slog.WarnContext(ctx, "DescribeInstances: Instance bucket query returned error", "topic", topic, "code", responseError.Code)
		return nil, false
	}
	var output ec2.DescribeInstancesOutput
	if err := json.Unmarshal(msg.Data, &output); err != nil {
		slog.ErrorContext(ctx, "DescribeInstances: Failed to unmarshal instance bucket response", "topic", topic, "err", err)
		return nil, false
	}
	if len(output.Reservations) > 0 {
		slog.InfoContext(ctx, "DescribeInstances: Collected reservations from bucket", "topic", topic, "count", len(output.Reservations))
	}
	return output.Reservations, true
}
