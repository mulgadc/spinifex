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

// DescribeInstances queries all spinifex nodes for their instances via NATS
// and aggregates the results into a single response
func DescribeInstances(input *ec2.DescribeInstancesInput, natsConn *nats.Conn, expectedNodes int, accountID string) (*ec2.DescribeInstancesOutput, error) {
	// Marshal input to JSON
	jsonData, err := json.Marshal(input)
	if err != nil {
		slog.Error("DescribeInstances: Failed to marshal input", "err", err)
		return nil, fmt.Errorf("failed to marshal input: %w", err)
	}

	// Create an inbox for collecting responses from all nodes
	inbox := nats.NewInbox()
	sub, err := natsConn.SubscribeSync(inbox)
	if err != nil {
		slog.Error("DescribeInstances: Failed to create inbox subscription", "err", err)
		return nil, fmt.Errorf("failed to create inbox: %w", err)
	}
	defer sub.Unsubscribe()

	// Publish request to all nodes with account ID header
	pubMsg := nats.NewMsg("ec2.DescribeInstances")
	pubMsg.Reply = inbox
	pubMsg.Data = jsonData
	pubMsg.Header.Set(utils.AccountIDHeader, accountID)
	err = natsConn.PublishMsg(pubMsg)
	if err != nil {
		slog.Error("DescribeInstances: Failed to publish request", "err", err)
		return nil, fmt.Errorf("failed to publish request: %w", err)
	}

	// Collect responses from all nodes
	// Timeout serves as a safety mechanism in case some nodes don't respond
	timeout := 3 * time.Second
	deadline := time.Now().Add(timeout)

	var allReservations []*ec2.Reservation
	var clientError string // first client error code from any node (e.g. InvalidParameterValue)
	responsesReceived := 0

	// If expectedNodes is not configured (0), fall back to timeout-based collection
	if expectedNodes <= 0 {
		expectedNodes = -1 // Disable early exit
		slog.Warn("DescribeInstances: ExpectedNodes not configured, using timeout-only collection")
	}

	for time.Now().Before(deadline) {
		// Check if we've received responses from all expected nodes
		if expectedNodes > 0 && responsesReceived >= expectedNodes {
			slog.Info("DescribeInstances: Received responses from all expected nodes", "expected", expectedNodes, "received", responsesReceived)
			break
		}

		// Calculate remaining timeout
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}

		// Wait for next message with remaining timeout
		msg, err := sub.NextMsg(remaining)
		if err != nil {
			if err == nats.ErrTimeout {
				// Timeout reached, no more messages
				break
			}
			slog.Error("DescribeInstances: Error receiving message", "err", err)
			break
		}

		// Increment response counter (even for errors, as we heard from the node)
		responsesReceived++

		// Check if response is an error
		responseError, err := utils.ValidateErrorPayload(msg.Data)
		if err != nil {
			code := ""
			if responseError.Code != nil {
				code = *responseError.Code
			}
			// Capture the first client error (e.g. InvalidParameterValue). Client errors
			// are deterministic — all nodes return the same error for the same invalid
			// request — so we propagate them to the caller after collection completes.
			if clientError == "" && code != "" {
				if info, known := awserrors.ErrorLookup[code]; known && info.HTTPCode >= 400 && info.HTTPCode < 500 {
					clientError = code
				}
			}
			slog.Warn("DescribeInstances: Received error from node", "code", code, "responses_received", responsesReceived)
			continue
		}

		// Parse the DescribeInstancesOutput from this node
		var nodeOutput ec2.DescribeInstancesOutput
		err = json.Unmarshal(msg.Data, &nodeOutput)
		if err != nil {
			slog.Error("DescribeInstances: Failed to unmarshal node response", "err", err)
			continue
		}

		// Aggregate reservations from this node
		if nodeOutput.Reservations != nil {
			allReservations = append(allReservations, nodeOutput.Reservations...)
			slog.Info("DescribeInstances: Collected reservations from node", "count", len(nodeOutput.Reservations), "responses_received", responsesReceived)
		}
	}

	// Query stopped and terminated instances in parallel (both use queue groups — single responder each)
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

	// If every node returned a client error and we collected no data, propagate
	// the error to the caller so the HTTP response carries the correct status.
	if clientError != "" && len(allReservations) == 0 {
		return nil, errors.New(clientError)
	}

	// Build final aggregated response
	output := &ec2.DescribeInstancesOutput{
		Reservations: allReservations,
	}

	slog.Info("DescribeInstances: Aggregated response", "total_reservations", len(allReservations))
	return output, nil
}

// EnrichInstanceProfileIDs fills Instance.IamInstanceProfile.Id for every
// instance whose daemon-side payload carried only the profile ARN. Daemons
// have no IAM access, so they emit Arn only; the gateway resolves Id via
// IAMService per unique ARN (cached to avoid one RPC per instance). Misses
// are warn-logged and leave Id empty per the AWS contract — this happens
// when an instance still references a deleted profile (graceful degradation).
//
// Safe to call with a nil output or nil IAMService (no-op).
func EnrichInstanceProfileIDs(out *ec2.DescribeInstancesOutput, iamSvc handlers_iam.IAMService, accountID string) {
	if out == nil || iamSvc == nil {
		return
	}
	cache := map[string]string{} // ARN → InstanceProfileID; "" means miss
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

// queryInstanceBucket sends a NATS request to a describe topic and returns the reservations.
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
