package gateway_ec2_instance

import (
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
func DescribeInstances(input *ec2.DescribeInstancesInput, natsConn *nats.Conn, expectedNodes int, accountID string) (*ec2.DescribeInstancesOutput, error) {
	jsonData, err := json.Marshal(input)
	if err != nil {
		slog.Error("DescribeInstances: Failed to marshal input", "err", err)
		return nil, fmt.Errorf("failed to marshal input: %w", err)
	}

	frames, sum, err := utils.Gather(natsConn, "ec2.DescribeInstances", jsonData,
		utils.GatherOpts{Timeout: 3 * time.Second, ExpectedNodes: expectedNodes, AccountID: accountID})
	if err != nil {
		return nil, err
	}

	var allReservations []*ec2.Reservation
	for _, frame := range frames {
		var nodeOutput ec2.DescribeInstancesOutput
		if json.Unmarshal(frame, &nodeOutput) == nil && nodeOutput.Reservations != nil {
			allReservations = append(allReservations, nodeOutput.Reservations...)
		}
	}

	// Both topics use queue groups (single responder each); query in parallel.
	var kvMu sync.Mutex
	var kvWg sync.WaitGroup
	for _, topic := range []string{"ec2.DescribeStoppedInstances", "ec2.DescribeTerminatedInstances"} {
		kvWg.Add(1)
		go func(topic string) {
			defer kvWg.Done()
			reservations := queryInstanceBucket(natsConn, topic, jsonData, accountID)
			if len(reservations) > 0 {
				kvMu.Lock()
				allReservations = append(allReservations, reservations...)
				kvMu.Unlock()
			}
		}(topic)
	}
	kvWg.Wait()

	// Propagate a deterministic 4xx only when nothing was collected (fan-out + KV).
	if sum.FirstClient4xx != "" && len(allReservations) == 0 {
		return nil, errors.New(sum.FirstClient4xx)
	}

	output := &ec2.DescribeInstancesOutput{
		Reservations: allReservations,
	}

	slog.Info("DescribeInstances: Aggregated response", "total_reservations", len(allReservations))
	return output, nil
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

// queryInstanceBucket queries a single describe topic and returns its reservations.
func queryInstanceBucket(natsConn *nats.Conn, topic string, jsonData []byte, accountID string) []*ec2.Reservation {
	reqMsg := nats.NewMsg(topic)
	reqMsg.Data = jsonData
	reqMsg.Header.Set(utils.AccountIDHeader, accountID)
	msg, err := natsConn.RequestMsg(reqMsg, 3*time.Second)
	if err != nil {
		slog.Warn("DescribeInstances: Failed to query instance bucket", "topic", topic, "err", err)
		return nil
	}
	if responseError, parseErr := utils.ValidateErrorPayload(msg.Data); parseErr != nil {
		slog.Warn("DescribeInstances: Instance bucket query returned error", "topic", topic, "code", responseError.Code)
		return nil
	}
	var output ec2.DescribeInstancesOutput
	if err := json.Unmarshal(msg.Data, &output); err != nil {
		slog.Error("DescribeInstances: Failed to unmarshal instance bucket response", "topic", topic, "err", err)
		return nil
	}
	if len(output.Reservations) > 0 {
		slog.Info("DescribeInstances: Collected reservations from bucket", "topic", topic, "count", len(output.Reservations))
	}
	return output.Reservations
}
