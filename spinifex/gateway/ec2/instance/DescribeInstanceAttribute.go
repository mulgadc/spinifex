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

	inbox := nats.NewInbox()
	sub, err := natsConn.SubscribeSync(inbox)
	if err != nil {
		slog.Error("DescribeInstanceAttribute: Failed to create inbox subscription", "err", err)
		return nil, fmt.Errorf("failed to create inbox: %w", err)
	}
	defer sub.Unsubscribe()

	pubMsg := nats.NewMsg("ec2.DescribeInstanceAttribute")
	pubMsg.Reply = inbox
	pubMsg.Data = jsonData
	pubMsg.Header.Set(utils.AccountIDHeader, accountID)
	if err := natsConn.PublishMsg(pubMsg); err != nil {
		slog.Error("DescribeInstanceAttribute: Failed to publish request", "err", err)
		return nil, fmt.Errorf("failed to publish request: %w", err)
	}

	deadline := time.Now().Add(3 * time.Second)

	if expectedNodes <= 0 {
		expectedNodes = -1
		slog.Warn("DescribeInstanceAttribute: ExpectedNodes not configured, using timeout-only collection")
	}

	var (
		success       *ec2.DescribeInstanceAttributeOutput
		clientErr     string // first non-NotFound AWS error seen
		notFoundCount int
		responses     int
	)

	for time.Now().Before(deadline) {
		if expectedNodes > 0 && responses >= expectedNodes {
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
			slog.Error("DescribeInstanceAttribute: Error receiving message", "err", err)
			break
		}
		responses++

		// ValidateErrorPayload returns a non-nil error when the payload is an
		// ec2.ResponseError envelope (the daemon's GenerateErrorPayload output);
		// nil means a normal success payload.
		if respErr, perr := utils.ValidateErrorPayload(msg.Data); perr != nil {
			code := ""
			if respErr.Code != nil {
				code = *respErr.Code
			}
			if code == awserrors.ErrorInvalidInstanceIDNotFound {
				notFoundCount++
				continue
			}
			if clientErr == "" && code != "" {
				clientErr = code
			}
			continue
		}

		// Skip success parsing once we already have one — the daemon that
		// owns the instance is unique, so the first parsed payload wins.
		if success != nil {
			continue
		}
		var out ec2.DescribeInstanceAttributeOutput
		if err := json.Unmarshal(msg.Data, &out); err != nil {
			slog.Error("DescribeInstanceAttribute: Failed to unmarshal node response", "err", err)
			continue
		}
		success = &out
	}

	if success != nil {
		slog.Info("DescribeInstanceAttribute: Completed successfully",
			"instance_id", *input.InstanceId, "responses", responses, "notfound", notFoundCount)
		return success, nil
	}
	if clientErr != "" {
		return nil, errors.New(clientErr)
	}
	if notFoundCount > 0 {
		return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	}
	// No responses at all — surface NotFound so terraform retries cleanly.
	slog.Warn("DescribeInstanceAttribute: No responses from any daemon",
		"instance_id", *input.InstanceId, "expected", expectedNodes)
	return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
}
