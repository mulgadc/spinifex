package gateway_ec2_instance

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// DescribeInstanceTypes fans out to all nodes and aggregates instance type info.
func DescribeInstanceTypes(input *ec2.DescribeInstanceTypesInput, natsConn *nats.Conn, expectedNodes int) (*ec2.DescribeInstanceTypesOutput, error) {
	jsonData, err := json.Marshal(input)
	if err != nil {
		slog.Error("DescribeInstanceTypes: Failed to marshal input", "err", err)
		return nil, fmt.Errorf("failed to marshal input: %w", err)
	}

	inbox := nats.NewInbox()
	sub, err := natsConn.SubscribeSync(inbox)
	if err != nil {
		slog.Error("DescribeInstanceTypes: Failed to create inbox subscription", "err", err)
		return nil, fmt.Errorf("failed to create inbox: %w", err)
	}
	defer sub.Unsubscribe()

	// No queue group — all daemons receive the request.
	err = natsConn.PublishRequest("ec2.DescribeInstanceTypes", inbox, jsonData)
	if err != nil {
		slog.Error("DescribeInstanceTypes: Failed to publish request", "err", err)
		return nil, fmt.Errorf("failed to publish request: %w", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	var allInstanceTypes []*ec2.InstanceTypeInfo
	responsesReceived := 0

	if expectedNodes <= 0 {
		expectedNodes = -1
		slog.Warn("DescribeInstanceTypes: ExpectedNodes not configured, using timeout-only collection")
	}

	for time.Now().Before(deadline) {
		if expectedNodes > 0 && responsesReceived >= expectedNodes {
			slog.Info("DescribeInstanceTypes: Received responses from all expected nodes", "expected", expectedNodes, "received", responsesReceived)
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
			slog.Error("DescribeInstanceTypes: Error receiving message", "err", err)
			break
		}
		responsesReceived++

		responseError, err := utils.ValidateErrorPayload(msg.Data)
		if err != nil {
			slog.Warn("DescribeInstanceTypes: Received error from node", "code", responseError.Code, "responses_received", responsesReceived)
			continue
		}

		var nodeOutput ec2.DescribeInstanceTypesOutput
		err = json.Unmarshal(msg.Data, &nodeOutput)
		if err != nil {
			slog.Error("DescribeInstanceTypes: Failed to unmarshal node response", "err", err)
			continue
		}

		if nodeOutput.InstanceTypes != nil {
			allInstanceTypes = append(allInstanceTypes, nodeOutput.InstanceTypes...)
			slog.Info("DescribeInstanceTypes: Collected instance types from node", "count", len(nodeOutput.InstanceTypes), "responses_received", responsesReceived)
		}
	}

	// capacity=true filter shows all slots (including duplicates) across nodes.
	showCapacity := false
	for _, f := range input.Filters {
		if f.Name != nil && *f.Name == "capacity" {
			for _, v := range f.Values {
				if v != nil && *v == "true" {
					showCapacity = true
					break
				}
			}
		}
	}

	requestedTypes := make(map[string]bool)
	for _, it := range input.InstanceTypes {
		if it != nil {
			requestedTypes[*it] = true
		}
	}

	var finalInstanceTypes []*ec2.InstanceTypeInfo
	if showCapacity {
		finalInstanceTypes = allInstanceTypes
	} else {
		seen := make(map[string]bool)
		for _, it := range allInstanceTypes {
			if it != nil && it.InstanceType != nil {
				if !seen[*it.InstanceType] {
					seen[*it.InstanceType] = true
					finalInstanceTypes = append(finalInstanceTypes, it)
				}
			}
		}
	}

	if len(requestedTypes) > 0 {
		var filtered []*ec2.InstanceTypeInfo
		for _, it := range finalInstanceTypes {
			if it != nil && it.InstanceType != nil && requestedTypes[*it.InstanceType] {
				filtered = append(filtered, it)
			}
		}
		finalInstanceTypes = filtered
	}

	output := &ec2.DescribeInstanceTypesOutput{
		InstanceTypes: finalInstanceTypes,
	}

	slog.Info("DescribeInstanceTypes: Aggregated response", "total_instance_types", len(finalInstanceTypes), "show_capacity", showCapacity)
	return output, nil
}
