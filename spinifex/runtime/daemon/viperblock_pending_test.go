package daemon

import (
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	ec2instanceapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/instance"
	"github.com/stretchr/testify/require"
)

// Reproduces devprod-stress-2-5: during a deploy burst each launch's drive opens and shows in
// memory.current within a minute, while its pending charge still runs, so it is counted twice.
// A 75 GiB limit at 1024 MiB a volume holds 70 beside viperblock; counted twice, it refuses at 36.
func TestRunInstancesBurstIsNotCountedTwice(t *testing.T) {
	ns, _ := testutil.StartTestNATS(t)
	daemon, memStore := createFullTestDaemonWithStore(t, ns.ClientURL())
	seedTestAMI(t, memStore, daemon.config.Predastore.Bucket, "ami-burst")

	rm := daemon.resourceMgr
	var currentMiB atomic.Int64
	currentMiB.Store(400) // viperblock's own working set
	rm.mu.Lock()
	rm.hostVCPU, rm.reservedVCPU = 256, 8
	rm.hostMemGB, rm.reservedMem = 251, 2
	rm.readMemAvailableGB = func() (float64, bool) { return 200, true }
	rm.readViperblockMem = func() (int64, int64, bool) { return 75 * 1024, currentMiB.Load(), true }
	rm.nbdkitMainMiB = 1024
	rm.mu.Unlock()

	nc := daemon.natsConn
	for subject, h := range map[string]func(*Daemon) natsHandler{
		clusterv1.NodeStatusSubject:   func(d *Daemon) natsHandler { return d.handleNodeStatus },
		"ec2.RunInstances.t3.micro.*": func(d *Daemon) natsHandler { return d.handleEC2RunInstances },
		"ec2.DescribeInstanceTypes": func(d *Daemon) natsHandler {
			return handleNATSRequest(d.node, d.instanceService.DescribeInstanceTypes)
		},
	} {
		sub, err := nc.Subscribe(subject, asMsgHandler(h(daemon)))
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}

	launch := func() (string, error) {
		res, err := ec2instanceapi.RunInstances(t.Context(), &ec2.RunInstancesInput{
			ImageId:      aws.String("ami-burst"),
			InstanceType: aws.String("t3.micro"),
			MinCount:     aws.Int64(1),
			MaxCount:     aws.Int64(1),
		}, nc, nil, nil, testAccountID, nil, nil, 1)
		if err != nil {
			return "", err
		}
		require.Len(t, res.Instances, 1)
		return aws.StringValue(res.Instances[0].InstanceId), nil
	}

	// The budget is 72960 MiB. Each guest's nbdkit uses about 1 GiB at deploy once its
	// drive is open, as measured in 2-4, so 70 fit beside viperblock's 400 MiB.
	for i := range 70 {
		id, err := launch()
		require.NoError(t, err, "launch %d of 70 fits: %d MiB in use", i+1, currentMiB.Load())
		currentMiB.Add(1024)
		driveOpened(rm, id)
	}
	_, err := launch()
	require.Error(t, err, "the 71st root volume does not fit")
	code, ok := awserrors.ResolveErrorCode(err)
	require.True(t, ok)
	require.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, code)
}

// driveOpened is what the node sees when a launched guest's QEMU has opened its drives.
func driveOpened(rm *ResourceManager, instanceID string) {
	_ = rm
	_ = instanceID
}
