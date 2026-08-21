package gateway_ec2_instance

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// monitoringRequestTimeout bounds the round trip. The daemon fans out one
// owner request per instance concurrently and each of those is capped well
// below this, so the budget does not scale with the instance count.
const monitoringRequestTimeout = 30 * time.Second

// validateMonitoringInstanceIDs rejects an empty or malformed id list before
// anything reaches NATS.
func validateMonitoringInstanceIDs(instanceIDs []*string) error {
	if len(instanceIDs) == 0 {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	for _, id := range instanceIDs {
		if id == nil || !strings.HasPrefix(*id, "i-") {
			return errors.New(awserrors.ErrorInvalidInstanceIDMalformed)
		}
	}
	return nil
}

// MonitorInstances moves the given instances to the 60s detailed telemetry
// tier. The owning daemon applies it, so an instance running on any node or
// sitting stopped in shared KV is reachable.
func MonitorInstances(ctx context.Context, input *ec2.MonitorInstancesInput, natsConn *nats.Conn, accountID string) (*ec2.MonitorInstancesOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if err := validateMonitoringInstanceIDs(input.InstanceIds); err != nil {
		return nil, err
	}
	return utils.NATSRequest[ec2.MonitorInstancesOutput](ctx, natsConn, "ec2.MonitorInstances", input, monitoringRequestTimeout, accountID)
}

// UnmonitorInstances returns the given instances to the 300s basic tier.
func UnmonitorInstances(ctx context.Context, input *ec2.UnmonitorInstancesInput, natsConn *nats.Conn, accountID string) (*ec2.UnmonitorInstancesOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if err := validateMonitoringInstanceIDs(input.InstanceIds); err != nil {
		return nil, err
	}
	return utils.NATSRequest[ec2.UnmonitorInstancesOutput](ctx, natsConn, "ec2.UnmonitorInstances", input, monitoringRequestTimeout, accountID)
}
