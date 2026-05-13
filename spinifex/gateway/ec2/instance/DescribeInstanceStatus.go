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

const (
	notApplicable          = "not-applicable"
	reachabilityDetailName = "reachability"
)

// DescribeInstanceStatus fans out to every node and, with IncludeAllInstances,
// also augments from the stopped-instance KV bucket. The aggregator propagates
// the first deterministic 4xx only when no data was collected, matching
// DescribeInstances.
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
			statuses = append(statuses, buildInstanceStatusFromInstance(inst, az))
		}
	}
	return statuses
}

func buildInstanceStatusFromInstance(inst *ec2.Instance, az string) *ec2.InstanceStatus {
	state := &ec2.InstanceState{Name: aws.String(ec2.InstanceStateNameStopped)}
	if inst.State != nil {
		if inst.State.Name != nil {
			state.Name = inst.State.Name
		}
		state.Code = inst.State.Code
	}

	return &ec2.InstanceStatus{
		AvailabilityZone: aws.String(az),
		InstanceId:       inst.InstanceId,
		InstanceState:    state,
		InstanceStatus: &ec2.InstanceStatusSummary{
			Status: aws.String(notApplicable),
			Details: []*ec2.InstanceStatusDetails{{
				Name:   aws.String(reachabilityDetailName),
				Status: aws.String(notApplicable),
			}},
		},
		SystemStatus: &ec2.InstanceStatusSummary{
			Status: aws.String(notApplicable),
			Details: []*ec2.InstanceStatusDetails{{
				Name:   aws.String(reachabilityDetailName),
				Status: aws.String(notApplicable),
			}},
		},
	}
}

// First-writer-wins by InstanceId, so a running fan-out entry naturally beats a
// stale stopped-KV entry during stop/start race windows.
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
