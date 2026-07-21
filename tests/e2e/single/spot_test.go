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

// runSpotInstanceLifecycle exercises the two spot terminal transitions that the
// NATS-level integration tier (tests/integration/spot_instance_test.go) cannot
// observe without a real vm.Manager:
//
//   - Cancelling a spot request must leave its backing VM running. DaemonLite
//     has no vm.Manager, so the integration tier can only infer this from the
//     cancelled SIR retaining its InstanceId; this test observes the VM
//     directly via WaitForInstanceState(running).
//   - Terminating a spot-backed instance must drive the real async delegation
//     (TerminateInstances -> daemon teardown cleaner -> RemoveFromSpotRequest ->
//     CloseForInstance, daemon/vm_adapters.go instanceCleanerAdapter), not a
//     direct CloseForInstance call. That delegation needs a real vm.Manager to
//     run the teardown chain, which is out of DaemonLite's scope.
//
// The mock/CRUD/error-path coverage (describe, lineage write-back,
// insufficient-capacity) is not duplicated here; that already runs faster and
// without a live guest in TestRequestSpotInstances_Lifecycle (integration
// tier). A single RequestSpotInstances call for 2 instances is the minimum
// that lets this test drive both terminal transitions without a second
// boot/request cycle.
func runSpotInstanceLifecycle(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — Spot Instance terminal transitions (live VM)")

	amiID := needAMI(t, fix)
	instType, _ := needInstanceTypeArch(t, fix)
	keyName, _ := needKeyPair(t, fix)

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
	sirInstance := make(map[string]string, count)
	for _, sir := range reqOut.SpotInstanceRequests {
		id := aws.StringValue(sir.SpotInstanceRequestId)
		require.NotEmpty(t, id, "empty SpotInstanceRequestId in response")
		require.Truef(t, strings.HasPrefix(id, "sir-"), "SIR id %q lacks sir- prefix", id)
		sirIDs = append(sirIDs, id)
		instID := aws.StringValue(sir.InstanceId)
		require.NotEmptyf(t, instID, "SIR %s has no InstanceId", id)
		sirInstance[id] = instID
	}
	harness.Detail(t, "sir_cancel", sirIDs[0], "sir_terminate", sirIDs[1])

	// Pre-register teardown of every launched VM before the first blocking wait,
	// so a fatal still tears the real instances down.
	launchedIDs := []string{sirInstance[sirIDs[0]], sirInstance[sirIDs[1]]}
	t.Cleanup(func() {
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

	cancelSIR, termSIR := sirIDs[0], sirIDs[1]
	cancelInst, termInst := sirInstance[cancelSIR], sirInstance[termSIR]

	for _, instID := range launchedIDs {
		harness.WaitForInstanceState(t, fix.AWS, instID, "running")
	}

	// Cancel one request: it flips to cancelled, and the instance itself must
	// keep running (cancel != terminate). CancelSpotInstanceRequests only moves
	// the SIR bucket entry and never calls into instance/VM state, so this is
	// observed directly off the VM rather than inferred from the cancelled
	// record still carrying an InstanceId.
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

	// Terminate the other request's instance: the real daemon teardown cleaner
	// (instanceCleanerAdapter.RemoveFromSpotRequest) scans the active bucket by
	// InstanceId and moves the SIR to closed. Driven here through the genuine
	// TerminateInstances -> async cleaner chain, not a direct CloseForInstance call.
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
