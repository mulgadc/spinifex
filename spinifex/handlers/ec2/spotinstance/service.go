package handlers_ec2_spotinstance

import "github.com/aws/aws-sdk-go/service/ec2"

// SpotInstanceService defines the daemon-side persistence operations for Spot
// Instance Requests routed over NATS. CloseForInstance is in-process only (it is
// invoked by the teardown cleaner) and lives on the concrete impl.
type SpotInstanceService interface {
	PutSpotInstanceRequests(input *PutSpotRequestsInput, accountID string) (*PutSpotRequestsOutput, error)
	DescribeSpotInstanceRequests(input *ec2.DescribeSpotInstanceRequestsInput, accountID string) (*ec2.DescribeSpotInstanceRequestsOutput, error)
	CancelSpotInstanceRequests(input *ec2.CancelSpotInstanceRequestsInput, accountID string) (*ec2.CancelSpotInstanceRequestsOutput, error)
}
