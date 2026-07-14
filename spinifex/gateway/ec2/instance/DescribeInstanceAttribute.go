package gateway_ec2_instance

import (
	"context"
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
func DescribeInstanceAttribute(ctx context.Context, input *ec2.DescribeInstanceAttributeInput, natsConn *nats.Conn, expectedNodes int, accountID string) (*ec2.DescribeInstanceAttributeOutput, error) {
	if err := ValidateDescribeInstanceAttributeInput(input); err != nil {
		return nil, err
	}

	slog.InfoContext(ctx, "DescribeInstanceAttribute: Processing request",
		"instance_id", *input.InstanceId, "attribute", *input.Attribute)

	jsonData, err := json.Marshal(input)
	if err != nil {
		slog.ErrorContext(ctx, "DescribeInstanceAttribute: Failed to marshal input", "err", err)
		return nil, fmt.Errorf("failed to marshal input: %w", err)
	}

	frames, sum, err := utils.Gather(ctx, natsConn, "ec2.DescribeInstanceAttribute", jsonData,
		utils.GatherOpts{Timeout: 3 * time.Second, ExpectedNodes: expectedNodes, StopOnFirst: true, AccountID: accountID})
	if err != nil {
		return nil, err
	}

	if len(frames) > 0 {
		var out ec2.DescribeInstanceAttributeOutput
		if json.Unmarshal(frames[0], &out) == nil {
			slog.InfoContext(ctx, "DescribeInstanceAttribute: Completed successfully",
				"instance_id", *input.InstanceId, "responses", sum.Received)
			return &out, nil
		}
	}

	// A node reported a real fault (a deterministic 4xx, or a 5xx such as a KV
	// outage) rather than confirming absence: surface it so the client retries
	// instead of treating a transient failure as a deleted instance.
	for code, n := range sum.ErrorCodes {
		if n > 0 && code != "" && code != awserrors.ErrorInvalidInstanceIDNotFound {
			return nil, errors.New(code)
		}
	}
	// Every node confirmed absence, or none answered — surface NotFound so
	// terraform retries cleanly.
	slog.WarnContext(ctx, "DescribeInstanceAttribute: instance absent or no responses",
		"instance_id", *input.InstanceId, "received", sum.Received, "expected", expectedNodes)
	return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
}
