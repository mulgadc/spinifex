package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// breakLocalStateWrite makes the daemon's local state write fail by occupying
// its parent directory path with a regular file, so MkdirAll returns ENOTDIR.
// Everything else rooted at DataDir keeps working.
func breakLocalStateWrite(t *testing.T, d *Daemon) {
	t.Helper()
	stateDir := filepath.Dir(d.localStatePath())
	require.NoError(t, os.RemoveAll(stateDir))
	require.NoError(t, os.WriteFile(stateDir, []byte("not a directory"), 0o600))
	require.Error(t, d.WriteState(), "state write must fail once the path is blocked")
}

func countVMs(d *Daemon) int {
	n := 0
	d.vmMgr.View(func(vms map[string]*vm.VM) { n = len(vms) })
	return n
}

// A launch whose local state write fails is reported as a failure and leaves
// nothing behind: no record in the manager, and the capacity it reserved is
// returned. The customer is never told "no" about an instance that exists.
func TestHandleEC2RunInstances_LocalWriteFailureLaunchesNothing(t *testing.T) {
	daemon, memStore := createFullTestDaemonWithStore(t, sharedNATSURL)
	seedTestAMI(t, memStore, daemon.config.Predastore.Bucket, "ami-writefail")

	instanceType := getTestInstanceType(t)
	typeInfo := daemon.resourceMgr.InstanceTypes()[instanceType]
	require.NotNil(t, typeInfo)
	capacityBefore := daemon.resourceMgr.CanAllocate(typeInfo, 64)

	sub, err := daemon.natsConn.QueueSubscribe("ec2.RunInstances.writefail", "spinifex-workers", asMsgHandler(daemon.handleEC2RunInstances))
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	breakLocalStateWrite(t, daemon)

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-writefail"),
		InstanceType: aws.String(instanceType),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	}
	reply, err := natsRequest(daemon.natsConn, "ec2.RunInstances.writefail", mustMarshal(t, input), 5*time.Second)
	require.NoError(t, err)

	assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"],
		"a launch that could not be recorded must be reported as an error: %s", reply.Data)

	var reservation ec2.Reservation
	if json.Unmarshal(reply.Data, &reservation) == nil {
		assert.Empty(t, reservation.Instances, "no instance may be returned to the caller")
	}

	assert.Zero(t, countVMs(daemon), "the failed launch must leave no record behind")
	assert.Equal(t, capacityBefore, daemon.resourceMgr.CanAllocate(typeInfo, 64),
		"capacity reserved for the failed launch must be returned")
}
