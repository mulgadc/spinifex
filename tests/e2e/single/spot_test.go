//go:build e2e

package single

import (
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runSpotInstanceLifecycle exercises the Spot Instance Request mock end to end.
// RequestSpotInstances synchronously launches real VMs via the on-demand path
// and reports the requests active/fulfilled — there is no bidding, interruption,
// or reclamation. One SIR is cancelled (request goes cancelled, instance keeps
// running); the other's instance is terminated (request moves to closed). A final
// oversized request asserts capacity failure persists no SIRs. Maps to the E2E
// row in docs/development/feature/spot-instances-v1.md.
func runSpotInstanceLifecycle(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — Spot Instance Requests (mock fulfilment)")

	amiID := needAMI(t, fix)
	instType, _ := needInstanceTypeArch(t, fix)
	keyName, _ := needKeyPair(t, fix)

	// InstanceCount=2 -> MinCount=MaxCount=2 (all-or-nothing). Two requests let
	// us drive both terminal transitions: cancel one, terminate the other.
	const count = 2
	harness.Step(t, "request-spot-instances ami=%s type=%s count=%d", amiID, instType, count)
	reqOut, err := fix.AWS.EC2.RequestSpotInstances(&ec2.RequestSpotInstancesInput{
		InstanceCount: aws.Int64(count),
		Type:          aws.String(ec2.SpotInstanceTypeOneTime),
		LaunchSpecification: &ec2.RequestSpotLaunchSpecification{
			ImageId:      aws.String(amiID),
			InstanceType: aws.String(instType),
			KeyName:      aws.String(keyName),
		},
	})
	require.NoError(t, err, "request-spot-instances --instance-count 2")
	require.Lenf(t, reqOut.SpotInstanceRequests, count,
		"expected %d spot instance requests, got %d", count, len(reqOut.SpotInstanceRequests))

	sirIDs := make([]string, 0, count)
	for _, sir := range reqOut.SpotInstanceRequests {
		id := aws.StringValue(sir.SpotInstanceRequestId)
		require.NotEmpty(t, id, "empty SpotInstanceRequestId in response")
		require.Truef(t, strings.HasPrefix(id, "sir-"), "SIR id %q lacks sir- prefix", id)
		sirIDs = append(sirIDs, id)
		assert.Equal(t, ec2.SpotInstanceStateActive, aws.StringValue(sir.State),
			"SIR %s should be active at create", id)
	}
	harness.Detail(t, "sir_cancel", sirIDs[0], "sir_terminate", sirIDs[1])

	// Pre-register teardown of every launched VM before the first blocking wait,
	// so a fatal still tears the real instances down.
	var launchedIDs []string
	t.Cleanup(func() {
		if len(launchedIDs) == 0 {
			return
		}
		_, _ = fix.AWS.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
			InstanceIds: aws.StringSlice(launchedIDs),
		})
	})

	// `aws ec2 wait spot-instance-request-fulfilled` — fulfilment is synchronous,
	// so the waiter returns on the first poll.
	harness.Step(t, "wait spot-instance-request-fulfilled")
	require.NoError(t, fix.AWS.EC2.WaitUntilSpotInstanceRequestFulfilled(&ec2.DescribeSpotInstanceRequestsInput{
		SpotInstanceRequestIds: aws.StringSlice(sirIDs),
	}), "wait spot-instance-request-fulfilled")

	// Describe both SIRs: active + fulfilled + an InstanceId mapped per request.
	descOut, err := fix.AWS.EC2.DescribeSpotInstanceRequests(&ec2.DescribeSpotInstanceRequestsInput{
		SpotInstanceRequestIds: aws.StringSlice(sirIDs),
	})
	require.NoError(t, err, "describe-spot-instance-requests")
	require.Lenf(t, descOut.SpotInstanceRequests, count,
		"expected %d SIRs from describe, got %d", count, len(descOut.SpotInstanceRequests))

	sirInstance := make(map[string]string, count)
	for _, sir := range descOut.SpotInstanceRequests {
		id := aws.StringValue(sir.SpotInstanceRequestId)
		assert.Equal(t, ec2.SpotInstanceStateActive, aws.StringValue(sir.State), "SIR %s state", id)
		require.NotNilf(t, sir.Status, "SIR %s missing status", id)
		assert.Equal(t, "fulfilled", aws.StringValue(sir.Status.Code), "SIR %s status code", id)
		instID := aws.StringValue(sir.InstanceId)
		require.NotEmptyf(t, instID, "SIR %s has no InstanceId", id)
		sirInstance[id] = instID
		launchedIDs = append(launchedIDs, instID)
	}

	// Both backing VMs are real on-demand launches — wait for running.
	for _, instID := range sirInstance {
		harness.WaitForInstanceState(t, fix.AWS, instID, "running")
	}

	cancelSIR, termSIR := sirIDs[0], sirIDs[1]
	cancelInst, termInst := sirInstance[cancelSIR], sirInstance[termSIR]
	require.NotEmpty(t, cancelInst, "cancel SIR resolved no instance")
	require.NotEmpty(t, termInst, "terminate SIR resolved no instance")

	// Cancel one request: it flips to cancelled but the instance keeps running
	// (cancel != terminate). The gateway->daemon move is synchronous.
	harness.Step(t, "cancel-spot-instance-requests %s", cancelSIR)
	cancelOut, err := fix.AWS.EC2.CancelSpotInstanceRequests(&ec2.CancelSpotInstanceRequestsInput{
		SpotInstanceRequestIds: aws.StringSlice([]string{cancelSIR}),
	})
	require.NoError(t, err, "cancel-spot-instance-requests %s", cancelSIR)
	require.Lenf(t, cancelOut.CancelledSpotInstanceRequests, 1,
		"expected 1 cancelled request, got %d", len(cancelOut.CancelledSpotInstanceRequests))
	assert.Equal(t, ec2.CancelSpotInstanceRequestStateCancelled,
		aws.StringValue(cancelOut.CancelledSpotInstanceRequests[0].State))

	cancelled := describeSpotRequest(t, fix, cancelSIR)
	assert.Equal(t, ec2.SpotInstanceStateCancelled, aws.StringValue(cancelled.State),
		"SIR %s should be cancelled", cancelSIR)
	require.NotNil(t, cancelled.Status)
	assert.Equal(t, "request-canceled-and-instance-running", aws.StringValue(cancelled.Status.Code))

	running := harness.WaitForInstanceState(t, fix.AWS, cancelInst, "running")
	assert.Equal(t, "running", aws.StringValue(running.State.Name),
		"cancelled SIR's instance %s must keep running", cancelInst)

	// Terminate the other request's instance: the daemon teardown cleaner scans
	// the active bucket by InstanceId and moves the SIR to closed.
	harness.Step(t, "terminate-instances %s (closes %s)", termInst, termSIR)
	_, err = fix.AWS.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
		InstanceIds: aws.StringSlice([]string{termInst}),
	})
	require.NoError(t, err, "terminate-instances %s", termInst)
	harness.WaitForInstanceState(t, fix.AWS, termInst, "terminated")

	// The close runs through the async teardown chain, so poll for it.
	var closed *ec2.SpotInstanceRequest
	harness.Eventually(t, func() bool {
		out, derr := fix.AWS.EC2.DescribeSpotInstanceRequests(&ec2.DescribeSpotInstanceRequestsInput{
			SpotInstanceRequestIds: aws.StringSlice([]string{termSIR}),
		})
		if derr != nil || len(out.SpotInstanceRequests) != 1 {
			return false
		}
		closed = out.SpotInstanceRequests[0]
		return aws.StringValue(closed.State) == ec2.SpotInstanceStateClosed
	}, 60*time.Second, 2*time.Second, "SIR %s should move to closed after terminate", termSIR)
	require.NotNil(t, closed, "describe never returned SIR %s", termSIR)
	require.NotNil(t, closed.Status)
	assert.Equal(t, "instance-terminated-by-user", aws.StringValue(closed.Status.Code))

	// Insufficient capacity: an oversized request short-circuits before any
	// launch (totalCapacity < MinCount) and persists no SIRs.
	t.Run("InsufficientCapacityNoSIRs", func(t *testing.T) {
		harness.Step(t, "request-spot-instances count=1000000 (capacity short-circuit)")
		out, rerr := fix.AWS.EC2.RequestSpotInstances(&ec2.RequestSpotInstancesInput{
			InstanceCount: aws.Int64(1_000_000),
			LaunchSpecification: &ec2.RequestSpotLaunchSpecification{
				ImageId:      aws.String(amiID),
				InstanceType: aws.String(instType),
				KeyName:      aws.String(keyName),
			},
		})
		harness.AssertAWSError(t, rerr, "InsufficientInstanceCapacity")
		if out != nil {
			assert.Empty(t, out.SpotInstanceRequests,
				"capacity failure must persist/return no SIRs")
		}
	})
}

// describeSpotRequest returns the single SIR matching sirID, failing the test if
// the gateway does not return exactly one.
func describeSpotRequest(t *testing.T, fix *Fixture, sirID string) *ec2.SpotInstanceRequest {
	t.Helper()
	out, err := fix.AWS.EC2.DescribeSpotInstanceRequests(&ec2.DescribeSpotInstanceRequestsInput{
		SpotInstanceRequestIds: aws.StringSlice([]string{sirID}),
	})
	require.NoError(t, err, "describe-spot-instance-requests %s", sirID)
	require.Lenf(t, out.SpotInstanceRequests, 1,
		"expected exactly 1 SIR for %s, got %d", sirID, len(out.SpotInstanceRequests))
	return out.SpotInstanceRequests[0]
}
