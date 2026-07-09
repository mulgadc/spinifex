package gateway_ec2_instance

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// DescribeInstanceTypes fans out to all nodes and aggregates instance type info.
func DescribeInstanceTypes(ctx context.Context, input *ec2.DescribeInstanceTypesInput, natsConn *nats.Conn, expectedNodes int, accountID string) (*ec2.DescribeInstanceTypesOutput, error) {
	jsonData, err := json.Marshal(input)
	if err != nil {
		slog.ErrorContext(ctx, "DescribeInstanceTypes: Failed to marshal input", "err", err)
		return nil, fmt.Errorf("failed to marshal input: %w", err)
	}

	frames, _, err := utils.Gather(ctx, natsConn, "ec2.DescribeInstanceTypes", jsonData,
		utils.GatherOpts{Timeout: 3 * time.Second, ExpectedNodes: expectedNodes, AccountID: accountID})
	if err != nil {
		return nil, err
	}

	var allInstanceTypes []*ec2.InstanceTypeInfo
	for _, frame := range frames {
		var nodeOutput ec2.DescribeInstanceTypesOutput
		if json.Unmarshal(frame, &nodeOutput) == nil {
			allInstanceTypes = append(allInstanceTypes, nodeOutput.InstanceTypes...)
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

	slog.InfoContext(ctx, "DescribeInstanceTypes: Aggregated response", "total_instance_types", len(finalInstanceTypes), "show_capacity", showCapacity)
	return output, nil
}
