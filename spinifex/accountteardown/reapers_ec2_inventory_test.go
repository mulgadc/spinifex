package accountteardown

//test:in-package — the instance reaper is unexported, and whether its listing
// can be mistaken for an empty account is the safety property under test.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/internal/testkit"
	ec2instanceapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/instance"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var inventoryNodes = []string{"node-1", "node-2"}

// testInventoryReaper builds the instance reaper for a two-node cluster, with
// a short fan-out deadline so a silent node costs the test little.
func testInventoryReaper(nc *nats.Conn) *instanceReaper {
	return &instanceReaper{nc: nc, nodeIDs: inventoryNodes, describeOpts: []ec2instanceapi.DescribeOption{
		ec2instanceapi.WithFanoutTimeout(300 * time.Millisecond),
	}}
}

// answerAsNode replies on subject the way a daemon does, carrying the node ID
// header the fan-out attributes frames by.
func answerAsNode(t *testing.T, nc *nats.Conn, subject, nodeID string, payload []byte) {
	t.Helper()
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		reply := nats.NewMsg(msg.Reply)
		reply.Data = payload
		if nodeID != "" {
			reply.Header.Set(natsmsg.NodeIDHeader, nodeID)
		}
		_ = msg.RespondMsg(reply)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

func describeOutput(t *testing.T, instanceIDs ...string) []byte {
	t.Helper()
	out := ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{}}
	if len(instanceIDs) > 0 {
		reservation := &ec2.Reservation{ReservationId: aws.String("r-" + instanceIDs[0])}
		for _, id := range instanceIDs {
			reservation.Instances = append(reservation.Instances, &ec2.Instance{
				InstanceId: aws.String(id),
				State:      &ec2.InstanceState{Name: aws.String("running")},
			})
		}
		out.Reservations = append(out.Reservations, reservation)
	}
	payload, err := json.Marshal(out)
	require.NoError(t, err)
	return payload
}

// emptyInstanceBuckets answers the stopped and terminated bucket queries with
// nothing, so only the running-instance fan-out decides completeness.
func emptyInstanceBuckets(t *testing.T, nc *nats.Conn) {
	t.Helper()
	answerAsNode(t, nc, "ec2.DescribeStoppedInstances", "", describeOutput(t))
	answerAsNode(t, nc, "ec2.DescribeTerminatedInstances", "", describeOutput(t))
}

// A node that never answers is the timeout case: its running guests are absent
// from the aggregate, so an empty answer from the rest is not evidence the
// account has no instances.
func TestInstanceReaperFailsClosedWhenANodeDoesNotAnswer(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	emptyInstanceBuckets(t, nc)
	answerAsNode(t, nc, "ec2.DescribeInstances", "node-1", describeOutput(t))
	require.NoError(t, nc.Flush())

	found, err := testInventoryReaper(nc).List(testCtx(t), testAccountID)

	require.Error(t, err, "a listing missing a node must not read as an empty account, got %v", found)
	code, _, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorServiceUnavailable, code)
}

// The partial list is the dangerous shape: the reaper would terminate what it
// can see and then report the stage drained on the next, equally partial pass.
func TestInstanceReaperFailsClosedOnAPartialList(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	emptyInstanceBuckets(t, nc)
	answerAsNode(t, nc, "ec2.DescribeInstances", "node-1", describeOutput(t, "i-on-node-1"))
	require.NoError(t, nc.Flush())

	found, err := testInventoryReaper(nc).List(testCtx(t), testAccountID)

	require.Error(t, err, "a partial listing must not be returned as the inventory, got %v", found)
}

// An error frame from a node is no better than silence: it says nothing about
// what that node is running.
func TestInstanceReaperFailsClosedWhenANodeAnswersWithAnError(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	emptyInstanceBuckets(t, nc)
	answerAsNode(t, nc, "ec2.DescribeInstances", "node-1", describeOutput(t))
	answerAsNode(t, nc, "ec2.DescribeInstances", "node-2", awserrors.GenerateErrorPayload(awserrors.ErrorServerInternal))
	require.NoError(t, nc.Flush())

	_, err := testInventoryReaper(nc).List(testCtx(t), testAccountID)

	require.Error(t, err)
}

// Completeness is judged against the configured node set, not a count of
// replies: two frames from one node while another is silent is still missing
// a node.
func TestInstanceReaperFailsClosedWhenOneNodeAnswersTwice(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	emptyInstanceBuckets(t, nc)
	answerAsNode(t, nc, "ec2.DescribeInstances", "node-1", describeOutput(t))
	answerAsNode(t, nc, "ec2.DescribeInstances", "node-1", describeOutput(t))
	require.NoError(t, nc.Flush())

	_, err := testInventoryReaper(nc).List(testCtx(t), testAccountID)

	require.Error(t, err)
}

// Stopped instances live in a bucket, not on a node. A failed bucket query
// hides them as surely as a silent node hides running ones.
func TestInstanceReaperFailsClosedWhenTheStoppedBucketFails(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	answerAsNode(t, nc, "ec2.DescribeStoppedInstances", "", awserrors.GenerateErrorPayload(awserrors.ErrorServerInternal))
	answerAsNode(t, nc, "ec2.DescribeTerminatedInstances", "", describeOutput(t))
	answerAsNode(t, nc, "ec2.DescribeInstances", "node-1", describeOutput(t))
	answerAsNode(t, nc, "ec2.DescribeInstances", "node-2", describeOutput(t))
	require.NoError(t, nc.Flush())

	_, err := testInventoryReaper(nc).List(testCtx(t), testAccountID)

	require.Error(t, err)
}

// The healthy cluster still lists: every configured node answered, so the
// union is the account's inventory.
func TestInstanceReaperListsEveryNodeWhenAllAnswer(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	emptyInstanceBuckets(t, nc)
	answerAsNode(t, nc, "ec2.DescribeInstances", "node-1", describeOutput(t, "i-on-node-1"))
	answerAsNode(t, nc, "ec2.DescribeInstances", "node-2", describeOutput(t, "i-on-node-2"))
	require.NoError(t, nc.Flush())

	started := time.Now()
	found, err := testInventoryReaper(nc).List(testCtx(t), testAccountID)
	require.NoError(t, err)

	var ids []string
	for _, resource := range found {
		ids = append(ids, resource.ID)
	}
	assert.ElementsMatch(t, []string{"i-on-node-1", "i-on-node-2"}, ids)
	assert.Less(t, time.Since(started), 2*time.Second, "a fully answered fan-out must not wait out its deadline")
}

// End to end through the engine: with a node silent, the compute stage must
// not read as drained and the account record must survive.
func TestTeardownDoesNotReportComputeDrainedWhileANodeIsSilent(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	emptyInstanceBuckets(t, nc)
	answerAsNode(t, nc, "ec2.DescribeInstances", "node-1", describeOutput(t))
	require.NoError(t, nc.Flush())

	accounts := newFakeAccounts()
	volumes := &fakeReaper{kind: "volume", stage: StageStorage,
		resources: []Resource{{Kind: "volume", ID: "vol-held-by-node-2"}}}
	engine := quietEngine(accounts, testInventoryReaper(nc), volumes)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := engine.Teardown(ctx, request())

	require.Error(t, err, "teardown must not succeed on an incomplete instance inventory")
	assert.ErrorContains(t, err, "list instance")
	assert.False(t, accounts.deleted, "the account record must survive an incomplete inventory")
	for _, stage := range result.Stages {
		assert.NotEqual(t, StageCompute, stage.Stage, "the compute stage must not be reported finished")
	}
	assert.Equal(t, 0, volumes.deletesAttempted, "storage must not be touched before compute is proven drained")
}
