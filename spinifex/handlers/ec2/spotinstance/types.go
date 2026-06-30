package handlers_ec2_spotinstance

import (
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
)

const (
	// KVBucketSpotRequests holds open/active Spot Instance Requests; no TTL.
	KVBucketSpotRequests = "spinifex-spot-requests"
	// KVBucketSpotRequestsTerminal holds closed/cancelled SIRs; auto-expires after spotTerminalTTL.
	KVBucketSpotRequestsTerminal = "spinifex-spot-requests-terminal"
	// KVBucketSpotRequestsVersion is the schema version for both spot buckets.
	KVBucketSpotRequestsVersion = 1

	spotTerminalTTL = time.Hour
)

// Spot request status codes mirror AWS spot-request status codes.
const (
	// SpotStatusCodeFulfilled marks an active request whose instance is running.
	SpotStatusCodeFulfilled = "fulfilled"
	// SpotStatusCodeCanceledInstanceRunning marks a cancelled request whose instance keeps running.
	SpotStatusCodeCanceledInstanceRunning = "request-canceled-and-instance-running"
	// SpotStatusCodeInstanceTerminatedByUser marks a request closed because its instance was terminated.
	SpotStatusCodeInstanceTerminatedByUser = "instance-terminated-by-user"
)

// SpotRequestRecord is the persisted form of a Spot Instance Request.
// The full SDK object is JSON round-tripped to avoid re-mapping ~20 fields.
type SpotRequestRecord struct {
	AccountID string                   `json:"account_id"`
	Request   *ec2.SpotInstanceRequest `json:"request"`
}

// PutSpotRequestsInput carries Spot Instance Requests to persist in the active bucket.
type PutSpotRequestsInput struct {
	Requests []*ec2.SpotInstanceRequest `json:"requests"`
}

// PutSpotRequestsOutput is empty on success.
type PutSpotRequestsOutput struct{}
