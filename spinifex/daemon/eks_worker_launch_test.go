package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
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

// A stopped worker has no ec2.cmd.<id> owner (its per-instance subscription is
// torn down at stop). The terminate must fall back to the ec2.terminate queue
// group so TerminateStoppedInstance reaps the worker from shared KV — otherwise
// a stopped+wedged node's ENI pins the customer subnet/VPC undeletable and
// DeleteCluster wedges in DELETING (mulga-siv-475).
func TestTerminateWorkerInstances_StoppedFallbackToEC2Terminate(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	d := &Daemon{natsConn: nc}

	gotID := make(chan string, 1)
	sub, err := nc.Subscribe("ec2.terminate", func(msg *nats.Msg) {
		var req handlers_ec2_instance.TerminateStoppedInstanceInput
		_ = json.Unmarshal(msg.Data, &req)
		gotID <- req.InstanceID
		_ = msg.Respond([]byte(`{"status":"terminated"}`))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	require.NoError(t, d.TerminateWorkerInstances([]string{"i-stopped"}, "111122223333"))

	select {
	case id := <-gotID:
		assert.Equal(t, "i-stopped", id, "stopped worker must be reaped via ec2.terminate")
	case <-time.After(2 * time.Second):
		t.Fatal("expected stopped worker terminate routed to ec2.terminate")
	}
}

// A NotFound payload from the ec2.terminate fallback (worker already drained) is
// idempotent success, not a teardown failure.
func TestTerminateWorkerInstances_StoppedFallbackNotFoundIdempotent(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	d := &Daemon{natsConn: nc}

	sub, err := nc.Subscribe("ec2.terminate", func(msg *nats.Msg) {
		_ = msg.Respond(utils.GenerateErrorPayload(awserrors.ErrorInvalidInstanceIDNotFound))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	require.NoError(t, d.TerminateWorkerInstances([]string{"i-drained"}, "111122223333"))
}

// A non-NotFound error from the ec2.terminate fallback must surface so the
// teardown backstop retries rather than orphaning the worker (and its ENI).
func TestTerminateWorkerInstances_StoppedFallbackErrorSurfaces(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	d := &Daemon{natsConn: nc}

	sub, err := nc.Subscribe("ec2.terminate", func(msg *nats.Msg) {
		_ = msg.Respond(utils.GenerateErrorPayload(awserrors.ErrorOperationNotPermitted))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	err = d.TerminateWorkerInstances([]string{"i-protected-stopped"}, "111122223333")
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
