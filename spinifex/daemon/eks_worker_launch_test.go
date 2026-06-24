package daemon

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	spxtypes "github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A worker with no owner (no responder on ec2.cmd.<id>) is already-gone, so
// terminating it is an idempotent no-op success (a retried DeleteNodegroup /
// scale-down must not wedge on drained instances).
func TestTerminateWorkerInstances_NotFoundIsIdempotent(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	d := &Daemon{natsConn: nc}

	require.NoError(t, d.TerminateWorkerInstances([]string{"i-gone1", "i-gone2"}, "111122223333"))
	// Empty / blank IDs are skipped.
	require.NoError(t, d.TerminateWorkerInstances([]string{"", ""}, "111122223333"))
	require.NoError(t, d.TerminateWorkerInstances(nil, "111122223333"))
}

// Worker terminate must route to whichever node owns the VM via ec2.cmd.<id>,
// not a local-only vmMgr lookup; the owner runs the full teardown (incl. ENI
// detach+delete) so no dangling ENI pins the VPC undeletable (mulga-siv-408).
func TestTerminateWorkerInstances_RoutesToOwner(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	d := &Daemon{natsConn: nc}

	gotCmd := make(chan spxtypes.EC2InstanceCommand, 1)
	sub, err := nc.Subscribe("ec2.cmd.i-owned", func(msg *nats.Msg) {
		var cmd spxtypes.EC2InstanceCommand
		_ = json.Unmarshal(msg.Data, &cmd)
		gotCmd <- cmd
		_ = msg.Respond([]byte(`{}`))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	require.NoError(t, d.TerminateWorkerInstances([]string{"i-owned"}, "111122223333"))

	select {
	case cmd := <-gotCmd:
		assert.Equal(t, "i-owned", cmd.ID)
		assert.True(t, cmd.Attributes.TerminateInstance, "owner must receive a terminate command")
		assert.True(t, cmd.Attributes.StopInstance, "StopInstance set so the owner does not restart it")
	case <-time.After(2 * time.Second):
		t.Fatal("expected terminate command routed to the owning node")
	}
}

// A NotFound error payload from the owner (instance already drained between
// enumerate and terminate) is idempotent success, not a teardown failure.
func TestTerminateWorkerInstances_OwnerNotFoundPayloadIdempotent(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	d := &Daemon{natsConn: nc}

	sub, err := nc.Subscribe("ec2.cmd.i-raced", func(msg *nats.Msg) {
		_ = msg.Respond(utils.GenerateErrorPayload(awserrors.ErrorInvalidInstanceIDNotFound))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	require.NoError(t, d.TerminateWorkerInstances([]string{"i-raced"}, "111122223333"))
}

// A non-NotFound error payload from the owner must surface so the teardown
// backstop retries rather than silently leaving the worker (and its ENI) alive.
func TestTerminateWorkerInstances_OwnerErrorSurfaces(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	d := &Daemon{natsConn: nc}

	sub, err := nc.Subscribe("ec2.cmd.i-protected", func(msg *nats.Msg) {
		_ = msg.Respond(utils.GenerateErrorPayload(awserrors.ErrorOperationNotPermitted))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	err = d.TerminateWorkerInstances([]string{"i-protected"}, "111122223333")
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorOperationNotPermitted)
}

// Both worker methods must guard against an uninitialized daemon rather than
// nil-panic.
func TestWorkerLauncher_NilInstanceService(t *testing.T) {
	d := &Daemon{}

	_, err := d.RunWorkerInstance(&ec2.RunInstancesInput{ImageId: aws.String("ami-1")}, "111122223333")
	require.Error(t, err)

	require.Error(t, d.TerminateWorkerInstances([]string{"i-1"}, "111122223333"))
}

// A node-targeted worker launch publishes the ec2.RunInstances.<type>.<node>
// request the owning node handles, base64-encoding user-data on the way out, so
// nodegroup workers spread across hosts instead of all landing locally.
func TestRunWorkerInstanceOnNode_RoutesToTargetNode(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	d := &Daemon{natsConn: nc}

	gotUserData := make(chan string, 1)
	sub, err := nc.Subscribe("ec2.RunInstances.t3.medium.nodeX", func(msg *nats.Msg) {
		var in ec2.RunInstancesInput
		_ = json.Unmarshal(msg.Data, &in)
		gotUserData <- aws.StringValue(in.UserData)
		res, _ := json.Marshal(ec2.Reservation{Instances: []*ec2.Instance{{InstanceId: aws.String("i-spread1")}}})
		_ = msg.Respond(res)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	in := &ec2.RunInstancesInput{
		InstanceType: aws.String("t3.medium"),
		ImageId:      aws.String("ami-1"),
		UserData:     aws.String("#cloud-config\n"),
	}
	res, err := d.RunWorkerInstanceOnNode("nodeX", in, "111122223333")
	require.NoError(t, err)
	require.Len(t, res.Instances, 1)
	assert.Equal(t, "i-spread1", aws.StringValue(res.Instances[0].InstanceId))

	select {
	case ud := <-gotUserData:
		assert.Equal(t, base64.StdEncoding.EncodeToString([]byte("#cloud-config\n")), ud,
			"user-data must be base64-encoded for the node-targeted launch")
	case <-time.After(2 * time.Second):
		t.Fatal("expected the worker launch routed to ec2.RunInstances.t3.medium.nodeX")
	}
}
