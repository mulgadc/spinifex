//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRequestSpotInstances_Lifecycle exercises the gateway's Spot Instance Request
// orchestration end to end: RequestSpotInstances validates the input, dispatches the shared
// on-demand RunInstances placement/quota path (spinifex/gateway/ec2/spotinstance/
// RequestSpotInstances.go), persists one active/fulfilled SpotInstanceRequest per launched
// instance via the daemon's spot service, and stamps spot lineage back onto each instance.
// Fulfilment itself is a documented mock (RequestSpotInstances.go:2 — no bidding,
// interruption, or reclamation), so this drives the request/describe/cancel/close state
// machine and the real placement/capacity logic, not a real guest boot: that on-demand
// lifecycle (RunInstances expansion, real VM running/terminated transitions) is already
// covered by TestRunInstances_MultiCountExpansion (this package) and the single-tier e2e
// baseline helpers (tests/e2e/single/baseline_test.go launchBaselineInstance,
// tests/e2e/single/helpers_test.go needInstance).
//
// The stubbed node offers capacity for exactly 2 of instanceType, so both the 2-instance
// request and the later 1,000,000-instance request resolve through distributeInstances' real
// totalCapacity accounting (placement.go) rather than the degenerate zero-node path
// documented on TestRunInstances_MultiCountExpansion.
func TestRequestSpotInstances_Lifecycle(t *testing.T) {
	gw := StartGateway(t)
	spotSvc := StartSpotDaemonLite(t, gw)

	const (
		instanceType = "t3.micro"
		nodeID       = "node-1"
		count        = 2
		amiID        = "ami-0123456789abcdef0"
	)

	statusResp := mustMarshal(t, &types.NodeStatusResponse{
		Node: nodeID,
		InstanceTypes: []types.InstanceTypeCap{
			{Name: instanceType, Available: count},
		},
	})
	gw.StubSubject(t, "spinifex.node.status", statusResp)

	// The oversized request at the end of this test drives distributeInstances into
	// InsufficientInstanceCapacity; RunInstances then disambiguates "no capacity" from
	// "unknown type" via a DescribeInstanceTypes round trip (RunInstances.go
	// isKnownInstanceType) before surfacing the error. Stubbing instanceType as known keeps
	// that disambiguation truthful instead of masking the intended error as InvalidInstanceType.
	gw.StubSubject(t, "ec2.DescribeInstanceTypes", mustMarshal(t, &ec2.DescribeInstanceTypesOutput{
		InstanceTypes: []*ec2.InstanceTypeInfo{{InstanceType: aws.String(instanceType)}},
	}))

	launchedInstanceIDs := []string{"i-spot-1", "i-spot-2"}

	// Subscribing directly to the per-node launch subject (rather than a StubSubject
	// canned reply) lets the test assert the gateway genuinely dispatched
	// MinCount=MaxCount=2, mirroring TestRunInstances_MultiCountExpansion's anti-tautology
	// check for the on-demand path.
	var gotMinCount, gotMaxCount int64
	launchSubject := fmt.Sprintf("ec2.RunInstances.%s.%s", instanceType, nodeID)
	launchSub, err := gw.NATSConn.Subscribe(launchSubject, func(msg *nats.Msg) {
		var nodeInput ec2.RunInstancesInput
		if jsonErr := json.Unmarshal(msg.Data, &nodeInput); jsonErr != nil {
			t.Errorf("unmarshal per-node RunInstances request: %v", jsonErr)
			return
		}
		gotMinCount = aws.Int64Value(nodeInput.MinCount)
		gotMaxCount = aws.Int64Value(nodeInput.MaxCount)
		_ = msg.Respond(mustMarshal(t, &ec2.Reservation{
			ReservationId: aws.String("r-spot"),
			Instances: []*ec2.Instance{
				{InstanceId: aws.String(launchedInstanceIDs[0])},
				{InstanceId: aws.String(launchedInstanceIDs[1])},
			},
		}))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = launchSub.Unsubscribe() })

	// RequestSpotInstances stamps spot lineage back onto each launched instance's owner
	// subject (ec2.cmd.{id}) asynchronously after the response. Capturing every command sent
	// lets the write-back be asserted against the real SIR ids RequestSpotInstances just
	// minted, rather than the hand-built fixture lineage_test.go uses in isolation.
	lineageCmds := make(chan types.EC2InstanceCommand, count)
	cmdSub, err := gw.NATSConn.Subscribe("ec2.cmd.>", func(msg *nats.Msg) {
		var cmd types.EC2InstanceCommand
		if jsonErr := json.Unmarshal(msg.Data, &cmd); jsonErr != nil {
			t.Errorf("unmarshal spot lineage command: %v", jsonErr)
			return
		}
		lineageCmds <- cmd
		_ = msg.Respond([]byte(`{}`))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cmdSub.Unsubscribe() })

	ec2c := gw.EC2Client(t)

	reqOut, err := ec2c.RequestSpotInstances(&ec2.RequestSpotInstancesInput{
		InstanceCount: aws.Int64(count),
		Type:          aws.String(ec2.SpotInstanceTypeOneTime),
		LaunchSpecification: &ec2.RequestSpotLaunchSpecification{
			ImageId:      aws.String(amiID),
			InstanceType: aws.String(instanceType),
		},
	})
	require.NoError(t, err, "request-spot-instances --instance-count 2")
	require.Lenf(t, reqOut.SpotInstanceRequests, count,
		"expected %d spot instance requests, got %d", count, len(reqOut.SpotInstanceRequests))

	// Proves distributeInstances genuinely expanded the spot count onto the per-node launch
	// (rather than the zero-daemon path, which never reaches this subject at all) exactly like
	// TestRunInstances_MultiCountExpansion proves for the on-demand call.
	assert.Equal(t, int64(count), gotMinCount, "gateway must dispatch MinCount=2 on the per-node launch")
	assert.Equal(t, int64(count), gotMaxCount, "gateway must dispatch MaxCount=2 on the per-node launch")

	sirIDs := make([]string, 0, count)
	sirForInstance := make(map[string]string, count)
	for _, sir := range reqOut.SpotInstanceRequests {
		id := aws.StringValue(sir.SpotInstanceRequestId)
		require.NotEmpty(t, id, "empty SpotInstanceRequestId in response")
		require.Truef(t, strings.HasPrefix(id, "sir-"), "SIR id %q lacks sir- prefix", id)
		sirIDs = append(sirIDs, id)
		assert.Equal(t, ec2.SpotInstanceStateActive, aws.StringValue(sir.State),
			"SIR %s should be active at create", id)
		instID := aws.StringValue(sir.InstanceId)
		require.NotEmptyf(t, instID, "SIR %s has no InstanceId", id)
		sirForInstance[instID] = id
	}

	// Describe both SIRs: active + fulfilled + an InstanceId mapped per request.
	descOut, err := ec2c.DescribeSpotInstanceRequests(&ec2.DescribeSpotInstanceRequestsInput{
		SpotInstanceRequestIds: aws.StringSlice(sirIDs),
	})
	require.NoError(t, err, "describe-spot-instance-requests")
	require.Lenf(t, descOut.SpotInstanceRequests, count,
		"expected %d SIRs from describe, got %d", count, len(descOut.SpotInstanceRequests))
	for _, sir := range descOut.SpotInstanceRequests {
		id := aws.StringValue(sir.SpotInstanceRequestId)
		assert.Equal(t, ec2.SpotInstanceStateActive, aws.StringValue(sir.State), "SIR %s state", id)
		require.NotNilf(t, sir.Status, "SIR %s missing status", id)
		assert.Equal(t, "fulfilled", aws.StringValue(sir.Status.Code), "SIR %s status code", id)
		assert.NotEmptyf(t, aws.StringValue(sir.InstanceId), "SIR %s has no InstanceId", id)
	}

	// The lineage write-back is best-effort and asynchronous; wait for both commands and
	// assert each carries the correct SIR id for its instance — the same link the live test
	// proved by reading InstanceLifecycle/SpotInstanceRequestId back off a real running VM.
	seen := 0
	deadline := time.After(5 * time.Second)
	for seen < count {
		select {
		case cmd := <-lineageCmds:
			wantSIR, ok := sirForInstance[cmd.ID]
			require.Truef(t, ok, "lineage command for unexpected instance %s", cmd.ID)
			assert.True(t, cmd.Attributes.SetSpotLineage, "instance %s command must set spot lineage", cmd.ID)
			require.NotNilf(t, cmd.SpotLineageData, "instance %s command missing spot lineage data", cmd.ID)
			assert.Equal(t, wantSIR, cmd.SpotLineageData.SpotInstanceRequestId,
				"instance %s should link back to its SIR", cmd.ID)
			seen++
		case <-deadline:
			t.Fatalf("timed out waiting for spot lineage write-back; got %d/%d", seen, count)
		}
	}

	cancelSIR, termSIR := sirIDs[0], sirIDs[1]

	// Cancel one request: it flips to cancelled but the instance keeps running (cancel !=
	// terminate) — CancelSpotInstanceRequests only moves the SIR bucket entry and never calls
	// into instance/VM state, so the InstanceId staying attached to the cancelled record is
	// the externally-visible proof that cancel leaves the instance alone.
	cancelOut, err := ec2c.CancelSpotInstanceRequests(&ec2.CancelSpotInstanceRequestsInput{
		SpotInstanceRequestIds: aws.StringSlice([]string{cancelSIR}),
	})
	require.NoError(t, err, "cancel-spot-instance-requests %s", cancelSIR)
	require.Lenf(t, cancelOut.CancelledSpotInstanceRequests, 1,
		"expected 1 cancelled request, got %d", len(cancelOut.CancelledSpotInstanceRequests))
	assert.Equal(t, ec2.CancelSpotInstanceRequestStateCancelled,
		aws.StringValue(cancelOut.CancelledSpotInstanceRequests[0].State))

	cancelled := describeOneSIR(t, ec2c, cancelSIR)
	assert.Equal(t, ec2.SpotInstanceStateCancelled, aws.StringValue(cancelled.State),
		"SIR %s should be cancelled", cancelSIR)
	require.NotNil(t, cancelled.Status)
	assert.Equal(t, "request-canceled-and-instance-running", aws.StringValue(cancelled.Status.Code))
	assert.NotEmpty(t, aws.StringValue(cancelled.InstanceId),
		"cancelled SIR must keep its InstanceId — the instance itself is untouched")

	// Terminate-triggered close: a live daemon's teardown cleaner calls CloseForInstance
	// in-process when a spot-backed instance is terminated (spinifex/daemon/vm_adapters.go
	// RemoveFromSpotRequest) — that one-line delegation is already covered by
	// TestRemoveFromSpotRequest_NoService_NoOp plus CloseForInstance's own unit tests
	// (spinifex/handlers/ec2/spotinstance/service_impl_test.go). What those unit tests can't
	// prove is that a SIR minted by the real gateway orchestration — a generated id, keyed by
	// the account resolved from a genuine SigV4-authenticated request — closes correctly; this
	// does, by calling CloseForInstance directly the same way the teardown cleaner would.
	termInstanceID, ok := instanceForSIR(sirForInstance, termSIR)
	require.Truef(t, ok, "could not resolve instance for SIR %s", termSIR)
	require.NoError(t, spotSvc.CloseForInstance(context.Background(), termInstanceID, gw.AccountID))

	closed := describeOneSIR(t, ec2c, termSIR)
	assert.Equal(t, ec2.SpotInstanceStateClosed, aws.StringValue(closed.State), "SIR %s should be closed", termSIR)
	require.NotNil(t, closed.Status)
	assert.Equal(t, "instance-terminated-by-user", aws.StringValue(closed.Status.Code))

	// Insufficient capacity: an oversized request short-circuits before any launch
	// (totalCapacity(2) < MinCount(1,000,000), the real placement.go accounting — not the
	// zero-node degenerate path, since a node genuinely answered spinifex.node.status) and
	// persists no SIRs.
	t.Run("InsufficientCapacityNoSIRs", func(t *testing.T) {
		out, err := ec2c.RequestSpotInstances(&ec2.RequestSpotInstancesInput{
			InstanceCount: aws.Int64(1_000_000),
			LaunchSpecification: &ec2.RequestSpotLaunchSpecification{
				ImageId:      aws.String(amiID),
				InstanceType: aws.String(instanceType),
			},
		})
		requireAWSErrorCode(t, err, "InsufficientInstanceCapacity")
		if out != nil {
			assert.Empty(t, out.SpotInstanceRequests, "capacity failure must persist/return no SIRs")
		}
	})
}

// instanceForSIR reverse-looks-up the instance ID mapped to sirID in a
// instanceID->SIRID map built from a RequestSpotInstances response.
func instanceForSIR(sirForInstance map[string]string, sirID string) (instanceID string, ok bool) {
	for instID, sir := range sirForInstance {
		if sir == sirID {
			return instID, true
		}
	}
	return "", false
}

// describeOneSIR returns the single SIR matching sirID, failing the test if
// the gateway does not return exactly one.
func describeOneSIR(t *testing.T, ec2c *ec2.EC2, sirID string) *ec2.SpotInstanceRequest {
	t.Helper()
	out, err := ec2c.DescribeSpotInstanceRequests(&ec2.DescribeSpotInstanceRequestsInput{
		SpotInstanceRequestIds: aws.StringSlice([]string{sirID}),
	})
	require.NoError(t, err, "describe-spot-instance-requests %s", sirID)
	require.Lenf(t, out.SpotInstanceRequests, 1,
		"expected exactly 1 SIR for %s, got %d", sirID, len(out.SpotInstanceRequests))
	return out.SpotInstanceRequests[0]
}
