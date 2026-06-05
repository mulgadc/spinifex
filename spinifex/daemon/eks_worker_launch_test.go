package daemon

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_ec2_instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
	"github.com/stretchr/testify/require"
)

// A worker whose ID is not tracked by the local vmMgr is already-gone, so
// terminating it is an idempotent no-op success (a retried DeleteNodegroup /
// scale-down must not wedge on drained instances).
func TestTerminateWorkerInstances_NotFoundIsIdempotent(t *testing.T) {
	d := newDaemonWithVMs()
	// Non-nil service passes the init guard; no VM matches the IDs so
	// StopOrTerminateInstance is never reached — the not-found branch returns
	// success.
	d.instanceService = &handlers_ec2_instance.InstanceServiceImpl{}

	require.NoError(t, d.TerminateWorkerInstances([]string{"i-gone1", "i-gone2"}, "111122223333"))
	// Empty / blank IDs are skipped.
	require.NoError(t, d.TerminateWorkerInstances([]string{"", ""}, "111122223333"))
	require.NoError(t, d.TerminateWorkerInstances(nil, "111122223333"))
}

// Both worker methods must guard against an uninitialized instance service
// rather than nil-panic.
func TestWorkerLauncher_NilInstanceService(t *testing.T) {
	d := &Daemon{}

	_, err := d.RunWorkerInstance(&ec2.RunInstancesInput{ImageId: aws.String("ami-1")}, "111122223333")
	require.Error(t, err)

	require.Error(t, d.TerminateWorkerInstances([]string{"i-1"}, "111122223333"))
}
