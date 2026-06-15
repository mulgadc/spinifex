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
	"github.com/mulgadc/spinifex/spinifex/awserrors"
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

	inbox := nats.NewInbox()
	sub, err := natsConn.SubscribeSync(inbox)
	if err != nil {
		slog.Error("DescribeInstances: Failed to create inbox subscription", "err", err)
		return nil, fmt.Errorf("failed to create inbox: %w", err)
	}
	defer sub.Unsubscribe()

	pubMsg := nats.NewMsg("ec2.DescribeInstances")
	pubMsg.Reply = inbox
	pubMsg.Data = jsonData
	pubMsg.Header.Set(utils.AccountIDHeader, accountID)
	err = natsConn.PublishMsg(pubMsg)
	if err != nil {
		slog.Error("DescribeInstances: Failed to publish request", "err", err)
		return nil, fmt.Errorf("failed to publish request: %w", err)
	}

	deadline := time.Now().Add(3 * time.Second)

	var allReservations []*ec2.Reservation
	var clientError string // first deterministic 4xx error code
	responsesReceived := 0

	if expectedNodes <= 0 {
		expectedNodes = -1
		slog.Warn("DescribeInstances: ExpectedNodes not configured, using timeout-only collection")
	}

	for time.Now().Before(deadline) {
		// Check if we've received responses from all expected nodes
		if expectedNodes > 0 && responsesReceived >= expectedNodes {
			slog.Info("DescribeInstances: Received responses from all expected nodes", "expected", expectedNodes, "received", responsesReceived)
			break
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		msg, err := sub.NextMsg(remaining)
		if err != nil {
			if err == nats.ErrTimeout {
				break
			}
			slog.Error("DescribeInstances: Error receiving message", "err", err)
			break
		}

		responsesReceived++

		responseError, err := utils.ValidateErrorPayload(msg.Data)
		if err != nil {
			code := ""
			if responseError.Code != nil {
				code = *responseError.Code
			}
			// Capture the first deterministic 4xx; propagated only if no data collected.
			if clientError == "" && code != "" {
				if info, known := awserrors.ErrorLookup[code]; known && info.HTTPCode >= 400 && info.HTTPCode < 500 {
					clientError = code
				}
			}
			slog.Warn("DescribeInstances: Received error from node", "code", code, "responses_received", responsesReceived)
			continue
		}

		var nodeOutput ec2.DescribeInstancesOutput
		err = json.Unmarshal(msg.Data, &nodeOutput)
		if err != nil {
			slog.Error("DescribeInstances: Failed to unmarshal node response", "err", err)
			continue
		}

		if nodeOutput.Reservations != nil {
			allReservations = append(allReservations, nodeOutput.Reservations...)
			slog.Info("DescribeInstances: Collected reservations from node", "count", len(nodeOutput.Reservations), "responses_received", responsesReceived)
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

	if clientError != "" && len(allReservations) == 0 {
		return nil, errors.New(clientError)
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
