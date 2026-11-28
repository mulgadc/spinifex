package daemon

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	ec2instanceapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/instance"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
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
	rm.ExportOpened(instanceID)
}

// readingRoom sets the reading to leave room for exactly n more root volumes.
func readingRoom(rm *ResourceManager, n int64) {
	limit, _, _ := rm.readViperblockMem()
	budget := limit - limit*exportMarginPct/100
	rm.readViperblockMem = func() (int64, int64, bool) { return limit, budget - n*768, true }
}

// A launch whose greeting comes after the 5-minute window must end its own charge
// only: a later launch's unopened drive stays charged while the reading binds.
func TestViperblockLateSettleKeepsAnotherLaunchCharged(t *testing.T) {
	rm, micro := gatedRM(t, 20)
	readingRoom(rm, 2)
	require.NoError(t, rm.allocate(micro))
	rm.BindExport("i-a")
	rm.mu.Lock()
	rm.bound["i-a"] = time.Now().Add(-exportPending - time.Minute)
	rm.mu.Unlock()
	assert.Equal(t, 1, rm.pendingExports(time.Now()), "a bound charge outlasts the unbound window")

	require.NoError(t, rm.allocate(micro))
	rm.BindExport("i-b")
	rm.ExportOpened("i-a")
	rm.ExportOpened("i-a")
	assert.Equal(t, 1, rm.pendingExports(time.Now()), "i-b is still opening")
	assert.Equal(t, 1, rm.CanAllocate(micro, 2), "the reading's two slots less i-b's")
}

// A drive that opens slowly stays charged until its greeting or the backstop.
func TestViperblockSlowOpenStaysChargedUntilTheBackstop(t *testing.T) {
	rm, micro := gatedRM(t, 20)
	require.NoError(t, rm.allocate(micro))
	rm.BindExport("i-slow")
	now := time.Now()
	assert.Equal(t, 1, rm.pendingExports(now.Add(exportPending+time.Minute)))
	assert.Equal(t, 0, rm.pendingExports(now.Add(exportOpenBound)))
}

// A start that failed leaves the instance stopped and its old charge bound; the next
// start's allocation replaces it, so its charge runs from its own admission.
func TestViperblockSecondStartBindsAFreshCharge(t *testing.T) {
	rm, micro := gatedRM(t, 20)
	require.NoError(t, rm.allocate(micro))
	rm.BindExport("i-s")
	rm.mu.Lock()
	rm.bound["i-s"] = time.Now().Add(-exportOpenBound + time.Minute)
	rm.mu.Unlock()
	rm.deallocate(micro)

	require.NoError(t, rm.allocate(micro))
	rm.BindExport("i-s")
	assert.Equal(t, 1, rm.pendingExports(time.Now().Add(2*time.Minute)),
		"the second start is charged past the first attempt's backstop")
	assert.Empty(t, rm.pending, "the allocation was bound")
}

// Only running proves a drive opened: a launch that reached it without its greeting call
// is swept and its slot reopens the queue, while a launch being terminated keeps its
// charge, since it can still race to QEMU start. Read through the daemon's VM state.
func TestViperblockSweepEndsOnlyARunningLaunchsCharge(t *testing.T) {
	rm, micro := gatedRM(t, 20)
	readingRoom(rm, 1)
	rm.instanceTypes = map[string]*ec2.InstanceTypeInfo{"t3.micro": micro}
	_, nc := testutil.StartTestNATS(t)
	rm.initSubscriptions(nc, func(*nats.Msg) string { return outcomeSuccess }, nil, "vb-node")
	d := &Daemon{vmMgr: vm.NewManager()}
	rm.exportLive = d.instanceCanOpenExport

	inst := &vm.VM{ID: "i-r", Status: vm.StatePending, InstanceType: "t3.micro"}
	d.vmMgr.Insert(inst)
	require.NoError(t, rm.allocate(micro))
	rm.BindExport(inst.ID)
	queueSubs, _ := countSubsBySuffix(rm.instanceSubs, ".vb-node")
	require.Equal(t, 0, queueSubs, "the launch took the last slot")

	for _, st := range []vm.InstanceState{vm.StatePending, vm.StateError, vm.StateShuttingDown} {
		d.vmMgr.UpdateState(inst.ID, func(v *vm.VM) { v.Status = st })
		rm.resyncExports(time.Now())
		queueSubs, _ = countSubsBySuffix(rm.instanceSubs, ".vb-node")
		require.Equal(t, 0, queueSubs, "a launch in %s keeps its charge", st)
	}

	d.vmMgr.UpdateState(inst.ID, func(v *vm.VM) { v.Status = vm.StateRunning })
	rm.resyncExports(time.Now())
	queueSubs, _ = countSubsBySuffix(rm.instanceSubs, ".vb-node")
	assert.Equal(t, 1, queueSubs, "a running launch's slot reopens the queue")
	assert.Zero(t, rm.pendingExports(time.Now()))
}

// A launch binds before the handler inserts the instance, so an instance the VM
// manager does not know keeps its charge until the backstop.
func TestViperblockSweepKeepsAnUninsertedLaunchCharged(t *testing.T) {
	rm, micro := gatedRM(t, 20)
	rm.exportLive = func(string) (bool, bool) { return false, false }
	require.NoError(t, rm.allocate(micro))
	rm.BindExport("i-new")
	now := time.Now()
	rm.sweepBoundExports(now.Add(10 * time.Minute))
	assert.Equal(t, 1, rm.pendingExports(now))
}
