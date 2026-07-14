package handlers_ec2_spotinstance

import (
	"context"

	"github.com/aws/aws-sdk-go/service/ec2"
)

// SpotInstanceService defines the daemon-side persistence operations for Spot
// Instance Requests routed over NATS. CloseForInstance is in-process only (it is
// invoked by the teardown cleaner) and lives on the concrete impl.
type SpotInstanceService interface {
	PutSpotInstanceRequests(ctx context.Context, input *PutSpotRequestsInput, accountID string) (*PutSpotRequestsOutput, error)
	DescribeSpotInstanceRequests(ctx context.Context, input *ec2.DescribeSpotInstanceRequestsInput, accountID string) (*ec2.DescribeSpotInstanceRequestsOutput, error)
	CancelSpotInstanceRequests(ctx context.Context, input *ec2.CancelSpotInstanceRequestsInput, accountID string) (*ec2.CancelSpotInstanceRequestsOutput, error)
}
