package gateway_ec2_instance

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// notApplicable is the AWS-reported status/reachability value for instances
// that are not in the running state.
const notApplicable = "not-applicable"

// DescribeInstanceStatus queries all spinifex nodes for the status of their
// running instances via NATS and aggregates the results. When the caller asks
// for IncludeAllInstances, the gateway also queries the stopped-instance KV
// bucket and synthesises an InstanceStatus entry for each stopped instance
// with status/reachability=not-applicable.
//
// Input validation (i- prefix, filter names) happens daemon-side via the
// service layer, matching the pattern used by every other Describe* endpoint.
// The aggregator captures the first deterministic 4xx any responder returns
// and propagates it only if no data was collected (mirrors DescribeInstances).
func DescribeInstanceStatus(input *ec2.DescribeInstanceStatusInput, natsConn *nats.Conn, expectedNodes int, accountID, az string) (*ec2.DescribeInstanceStatusOutput, error) {
	if input == nil {
		input = &ec2.DescribeInstanceStatusInput{}
	}

	jsonData, err := json.Marshal(input)
	if err != nil {
		slog.Error("DescribeInstanceStatus: Failed to marshal input", "err", err)
		return nil, fmt.Errorf("failed to marshal input: %w", err)
	}

	inbox := nats.NewInbox()
	sub, err := natsConn.SubscribeSync(inbox)
	if err != nil {
		slog.Error("DescribeInstanceStatus: Failed to create inbox subscription", "err", err)
		return nil, fmt.Errorf("failed to create inbox: %w", err)
	}
	defer sub.Unsubscribe()

	pubMsg := nats.NewMsg("ec2.DescribeInstanceStatus")
	pubMsg.Reply = inbox
	pubMsg.Data = jsonData
	pubMsg.Header.Set(utils.AccountIDHeader, accountID)
	if err := natsConn.PublishMsg(pubMsg); err != nil {
		slog.Error("DescribeInstanceStatus: Failed to publish request", "err", err)
		return nil, fmt.Errorf("failed to publish request: %w", err)
	}

	timeout := 3 * time.Second
	deadline := time.Now().Add(timeout)

	var allStatuses []*ec2.InstanceStatus
	var clientError string
	responsesReceived := 0

	if expectedNodes <= 0 {
		expectedNodes = -1
		slog.Warn("DescribeInstanceStatus: ExpectedNodes not configured, using timeout-only collection")
	}

	for time.Now().Before(deadline) {
		if expectedNodes > 0 && responsesReceived >= expectedNodes {
			slog.Info("DescribeInstanceStatus: Received responses from all expected nodes", "expected", expectedNodes, "received", responsesReceived)
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
			slog.Error("DescribeInstanceStatus: Error receiving message", "err", err)
			break
		}
		responsesReceived++

		if responseError, perr := utils.ValidateErrorPayload(msg.Data); perr != nil {
			code := ""
			if responseError.Code != nil {
				code = *responseError.Code
			}
			if clientError == "" && code != "" {
				if info, known := awserrors.ErrorLookup[code]; known && info.HTTPCode >= 400 && info.HTTPCode < 500 {
					clientError = code
				}
			}
			slog.Warn("DescribeInstanceStatus: Received error from node", "code", code, "responses_received", responsesReceived)
			continue
		}

		var nodeOutput ec2.DescribeInstanceStatusOutput
		if err := json.Unmarshal(msg.Data, &nodeOutput); err != nil {
			slog.Error("DescribeInstanceStatus: Failed to unmarshal node response", "err", err)
			continue
		}
		if len(nodeOutput.InstanceStatuses) > 0 {
			allStatuses = append(allStatuses, nodeOutput.InstanceStatuses...)
			slog.Info("DescribeInstanceStatus: Collected statuses from node", "count", len(nodeOutput.InstanceStatuses), "responses_received", responsesReceived)
		}
	}

	if aws.BoolValue(input.IncludeAllInstances) {
		if stopped := queryStoppedInstancesForStatus(natsConn, jsonData, accountID, az); len(stopped) > 0 {
			allStatuses = append(allStatuses, stopped...)
		}
	}

	finalStatuses := dedupStatuses(allStatuses)

	if clientError != "" && len(finalStatuses) == 0 {
		return nil, errors.New(clientError)
	}

	slog.Info("DescribeInstanceStatus: Aggregated response", "total_statuses", len(finalStatuses))
	return &ec2.DescribeInstanceStatusOutput{InstanceStatuses: finalStatuses}, nil
}

// queryStoppedInstancesForStatus reuses the DescribeInstances KV bucket helper
// and transforms each returned *ec2.Instance into an *ec2.InstanceStatus
// carrying status/reachability=not-applicable.
func queryStoppedInstancesForStatus(natsConn *nats.Conn, jsonData []byte, accountID, az string) []*ec2.InstanceStatus {
	reservations := queryInstanceBucket(natsConn, "ec2.DescribeStoppedInstances", jsonData, accountID)
	var statuses []*ec2.InstanceStatus
	for _, res := range reservations {
		if res == nil {
			continue
		}
		for _, inst := range res.Instances {
			if inst == nil || inst.InstanceId == nil {
				continue
			}
			statuses = append(statuses, buildInstanceStatusFromInstance(inst, "stopped", az))
		}
	}
	return statuses
}

// buildInstanceStatusFromInstance transforms a stopped-KV reservation entry
// into an InstanceStatus. Stopped/pending/etc. instances always report
// not-applicable for both status and reachability; AWS uses the same value
// when the instance is not running.
func buildInstanceStatusFromInstance(inst *ec2.Instance, fallbackStateName string, az string) *ec2.InstanceStatus {
	state := &ec2.InstanceState{}
	if inst.State != nil {
		if inst.State.Name != nil {
			state.SetName(*inst.State.Name)
		} else {
			state.SetName(fallbackStateName)
		}
		if inst.State.Code != nil {
			state.SetCode(*inst.State.Code)
		}
	} else {
		state.SetName(fallbackStateName)
	}

	return &ec2.InstanceStatus{
		AvailabilityZone: aws.String(az),
		InstanceId:       inst.InstanceId,
		InstanceState:    state,
		InstanceStatus: &ec2.InstanceStatusSummary{
			Status: aws.String(notApplicable),
			Details: []*ec2.InstanceStatusDetails{{
				Name:   aws.String("reachability"),
				Status: aws.String(notApplicable),
			}},
		},
		SystemStatus: &ec2.InstanceStatusSummary{
			Status: aws.String(notApplicable),
			Details: []*ec2.InstanceStatusDetails{{
				Name:   aws.String("reachability"),
				Status: aws.String(notApplicable),
			}},
		},
	}
}

// dedupStatuses removes duplicate InstanceStatus entries by InstanceId.
// First-writer-wins: running fan-out responses arrive before the stopped-KV
// query result, so a running entry naturally wins over a stale stopped entry
// during stop/start race windows.
func dedupStatuses(in []*ec2.InstanceStatus) []*ec2.InstanceStatus {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]*ec2.InstanceStatus, 0, len(in))
	for _, s := range in {
		if s == nil || s.InstanceId == nil {
			continue
		}
		id := *s.InstanceId
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, s)
	}
	return out
}
