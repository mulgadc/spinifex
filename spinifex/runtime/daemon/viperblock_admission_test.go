package daemon

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Reproduces devprod-stress-2-1: a 251 GiB node admits launches on host RAM while every
// nbdkit lives in viperblock's cgroup, capped at 15% of RAM. At ~50 open root volumes
// that cgroup is full, so the next launch must be refused, not admitted and later fenced.
func TestRunInstancesRefusedWhenViperblockMemoryIsFull(t *testing.T) {
	daemon, memStore := createFullTestDaemonWithStore(t, sharedNATSURL)
	seedTestAMI(t, memStore, daemon.config.Predastore.Bucket, "ami-vbfull")

	rm := daemon.resourceMgr
	micro := rm.InstanceTypes()["t3.micro"]
	require.NotNil(t, micro, "t3.micro must be a known type")

	// The shape of a dev-prod host: 256 CPUs, 251 GiB, plenty of CPU and host RAM left.
	rm.mu.Lock()
	rm.hostVCPU, rm.reservedVCPU = 256, 8
	rm.hostMemGB, rm.reservedMem = 251, 2
	rm.readMemAvailableGB = func() (float64, bool) { return 200, true }
	rm.mu.Unlock()

	// 15% of 251 GiB is 37.65 GiB; at 768 MiB per root volume, 50 exports fill it.
	for i := 0; i < 50; i++ {
		require.NoError(t, rm.Allocate(micro), "launch %d of 50 fits host CPU and RAM", i+1)
	}

	sub, err := daemon.natsConn.QueueSubscribe("ec2.RunInstances.vbfull", "spinifex-workers", asMsgHandler(daemon.handleEC2RunInstances))
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-vbfull"),
		InstanceType: aws.String("t3.micro"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	}
	reply, err := natsRequest(daemon.natsConn, "ec2.RunInstances.vbfull", mustMarshal(t, input), 10*time.Second)
	require.NoError(t, err)
	assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, decodeError(t, reply.Data)["Code"],
		"the 51st root volume does not fit viperblock's memory limit: %s", reply.Data)
}
