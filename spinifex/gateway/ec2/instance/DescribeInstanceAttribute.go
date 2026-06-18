package gateway_ec2_instance

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// ValidateDescribeInstanceAttributeInput validates the input for DescribeInstanceAttribute.
func ValidateDescribeInstanceAttributeInput(input *ec2.DescribeInstanceAttributeInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.InstanceId == nil || *input.InstanceId == "" {
		return errors.New(awserrors.ErrorInvalidInstanceIDMalformed)
	}
	if !strings.HasPrefix(*input.InstanceId, "i-") {
		return errors.New(awserrors.ErrorInvalidInstanceIDMalformed)
	}
	if input.Attribute == nil || *input.Attribute == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// DescribeInstanceAttribute fans the request out to every daemon and returns
// the first successful payload. Only the daemon that owns the instance can
// answer; all others reply ErrorInvalidInstanceIDNotFound because they only
// inspect per-daemon local state (vmMgr / stoppedStore). The aggregator drops
// those NotFound replies and surfaces a real success, falling back to
// ErrorInvalidInstanceIDNotFound only when every node confirmed the instance
// is absent.
func DescribeInstanceAttribute(input *ec2.DescribeInstanceAttributeInput, natsConn *nats.Conn, expectedNodes int, accountID string) (*ec2.DescribeInstanceAttributeOutput, error) {
	if err := ValidateDescribeInstanceAttributeInput(input); err != nil {
		return nil, err
	}

	slog.Info("DescribeInstanceAttribute: Processing request",
		"instance_id", *input.InstanceId, "attribute", *input.Attribute)

	jsonData, err := json.Marshal(input)
	if err != nil {
		slog.Error("DescribeInstanceAttribute: Failed to marshal input", "err", err)
		return nil, fmt.Errorf("failed to marshal input: %w", err)
	}

	frames, sum, err := utils.Gather(natsConn, "ec2.DescribeInstanceAttribute", jsonData,
		utils.GatherOpts{Timeout: 3 * time.Second, ExpectedNodes: expectedNodes, StopOnFirst: true, AccountID: accountID})
	if err != nil {
		return nil, err
	}

	if len(frames) > 0 {
		var out ec2.DescribeInstanceAttributeOutput
		if json.Unmarshal(frames[0], &out) == nil {
			slog.Info("DescribeInstanceAttribute: Completed successfully",
				"instance_id", *input.InstanceId, "responses", sum.Received)
			return &out, nil
		}
	}

	// Every node confirmed the instance is absent.
	if sum.Received > 0 && sum.ErrorCodes[awserrors.ErrorInvalidInstanceIDNotFound] == sum.Received {
		return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	}
	// A deterministic client error (e.g. malformed) outranks the NotFound fallback.
	if sum.FirstClient4xx != "" {
		return nil, errors.New(sum.FirstClient4xx)
	}
	// No responses at all — surface NotFound so terraform retries cleanly.
	slog.Warn("DescribeInstanceAttribute: No responses from any daemon",
		"instance_id", *input.InstanceId, "expected", expectedNodes)
	return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
}
