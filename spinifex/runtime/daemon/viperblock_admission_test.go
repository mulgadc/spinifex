package daemon

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2instance "github.com/mulgadc/spinifex/spinifex/domains/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	vmmock "github.com/mulgadc/spinifex/spinifex/runtime/compute/vm/mock"
	"github.com/nats-io/nats.go"
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
	// viperblock's MemoryHigh=15% of 251 GiB, holding 50 open root volumes at 768 MiB.
	rm.readViperblockMem = func() (int64, int64, bool) { return 251 * 1024 * 15 / 100, 50 * 768, true }
	rm.mu.Unlock()

	for i := range 50 {
		require.NoError(t, rm.allocateAdopted(micro), "guest %d of 50 already runs here", i+1)
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

func TestExportCount(t *testing.T) {
	// A 37.65 GiB limit less its 5% margin is 36,626 MiB: 47 root volumes at 768 MiB.
	limit := int64(251 * 1024 * 15 / 100)
	assert.Equal(t, 47, exportCount(100, limit, 0, 0, 0, 768), "an empty node fits 47")
	assert.Equal(t, 5, exportCount(5, limit, 0, 0, 0, 768), "a request under the limit is not clamped")
	assert.Equal(t, 7, exportCount(100, limit, 0, 40, 0, 768), "admitted exports count before they open")
	assert.Equal(t, 0, exportCount(100, limit, limit, 0, 0, 768), "a full cgroup admits none")
	// Three attached data volumes show in memory.current, so the next launch refuses sooner.
	assert.Equal(t, 4, exportCount(100, limit, 43*768, 40, 0, 768), "memory.current counts attached volumes")
	// With the reading binding, exports admitted too recently to show in it are added to it.
	assert.Equal(t, 2, exportCount(100, limit, 43*768, 40, 2, 768), "pending exports add to the reading")
	assert.Equal(t, 0, exportCount(0, limit, 0, 0, 0, 768))
}

func TestParseCgroupMiB(t *testing.T) {
	v, ok := parseCgroupMiB("40536432640\n")
	assert.True(t, ok)
	assert.Equal(t, int64(38658), v)
	for _, s := range []string{"max\n", "", "x", "-1"} {
		_, ok := parseCgroupMiB(s)
		assert.False(t, ok, "%q is no limit", s)
	}
}

// gatedRM is a node with ample CPU and host RAM and room in viperblock for exactly room more roots.
func gatedRM(t *testing.T, room int) (*ResourceManager, *ec2.InstanceTypeInfo) {
	t.Helper()
	rm := &ResourceManager{hostVCPU: 256, reservedVCPU: 8, hostMemGB: 251, reservedMem: 2, nbdkitMainMiB: 768,
		reservations: map[string]*capacityReservation{}}
	// budget = limit less 5%; a limit of 20*768*100/95 MiB leaves 20 slots.
	limit := int64(20*768*100/95) + 1
	rm.readViperblockMem = func() (int64, int64, bool) { return limit, 0, true }
	micro := &ec2.InstanceTypeInfo{InstanceType: aws.String("t3.micro"),
		VCpuInfo: &ec2.VCpuInfo{DefaultVCpus: aws.Int64(2)}, MemoryInfo: &ec2.MemoryInfo{SizeInMiB: aws.Int64(1024)}}
	for range 20 - room {
		require.NoError(t, rm.allocate(micro))
	}
	rm.pending = nil // the guests that fill the node are established, their exports open
	return rm, micro
}

func TestViperblockGateNamesItselfAndRefusesAtTheLimit(t *testing.T) {
	rm, micro := gatedRM(t, 0)
	n, gate := rm.admit(micro, 1)
	assert.Equal(t, 0, n)
	assert.Equal(t, "viperblock-memory", gate)
	assert.Error(t, rm.allocate(micro), "a node-targeted launch is refused at allocate")
}

func TestViperblockGateAdmitsOneOfTwoConcurrentLaunchesForTheLastSlot(t *testing.T) {
	rm, micro := gatedRM(t, 1)
	var ok, refused atomic.Int32
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			if rm.allocate(micro) == nil {
				ok.Add(1)
			} else {
				refused.Add(1)
			}
		})
	}
	wg.Wait()
	assert.Equal(t, int32(1), ok.Load())
	assert.Equal(t, int32(1), refused.Load())
}

// The reading, not the admitted charges, binds: attached volumes and viperblock's own
// working set put memory.current above the roots' charge. Admitted launches that have
// not opened yet must still be debited, so two launches cannot share one last slot.
func TestViperblockGateDebitsPendingLaunchesWhenTheReadingBinds(t *testing.T) {
	rm, micro := gatedRM(t, 20)
	limit, _, _ := rm.readViperblockMem()
	budget := limit - limit*exportMarginPct/100
	rm.readViperblockMem = func() (int64, int64, bool) { return limit, budget - 768, true }
	var ok atomic.Int32
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			if rm.allocate(micro) == nil {
				ok.Add(1)
			}
		})
	}
	wg.Wait()
	assert.Equal(t, int32(1), ok.Load(), "one slot by the reading admits one launch")
}

func TestViperblockGateExpiresPendingOnceTheReadingCanShowIt(t *testing.T) {
	rm, _ := gatedRM(t, 20)
	now := time.Now()
	rm.exportAdmitted(now.Add(-exportPending - time.Minute))
	rm.exportAdmitted(now)
	assert.Equal(t, 1, rm.pendingExports(now), "an admission older than exportPending is in the reading")
	assert.Equal(t, 0, rm.pendingExports(now.Add(exportPending)), "and every charge expires")
}

// A release cannot tell which launch it belongs to, so it leaves pending charges alone:
// terminating an established guest must not free the slot of a launch still opening.
func TestViperblockGateKeepsPendingChargesAcrossAnUnrelatedRelease(t *testing.T) {
	rm, micro := gatedRM(t, 20)
	limit, _, _ := rm.readViperblockMem()
	budget := limit - limit*exportMarginPct/100
	// The reading shows 10 established guests and room for exactly one more.
	rm.readViperblockMem = func() (int64, int64, bool) { return limit, budget - 768, true }
	require.NoError(t, rm.allocate(micro), "the launch takes the last slot and is pending")
	rm.deallocate(micro) // an established guest terminates; its memory leaves the reading later
	assert.Equal(t, 1, rm.pendingExports(time.Now()))
	assert.Equal(t, 0, rm.CanAllocate(micro, 1), "the pending launch still holds the last slot")
}

// The live-memory clamp must not return before the viperblock gate: a request that host
// memory cuts to a positive count is still cut by viperblock.
func TestViperblockGateAppliesAfterTheLiveMemoryClamp(t *testing.T) {
	rm, micro := gatedRM(t, 3)
	rm.readMemAvailableGB = func() (float64, bool) { return 2 + 5*1.75 + 0.1, true } // host memory for 5
	n, gate := rm.admit(micro, 10)
	assert.Equal(t, 3, n, "viperblock has room for 3")
	assert.Equal(t, "viperblock-memory", gate)
	rm.readMemAvailableGB = func() (float64, bool) { return 2 + 2*1.75 + 0.1, true } // host memory for 2
	n, gate = rm.admit(micro, 10)
	assert.Equal(t, 2, n)
	assert.Equal(t, "live-memory", gate)
}

// A full node drops its queue subscription, keeps the node-targeted one, and rejoins the
// queue when the reading falls, with no terminate: the resync timer's update.
func TestViperblockGateDropsAndRejoinsTheQueueSubscription(t *testing.T) {
	rm, micro := gatedRM(t, 20)
	limit, _, _ := rm.readViperblockMem()
	var full atomic.Bool
	rm.readViperblockMem = func() (int64, int64, bool) {
		if full.Load() {
			return limit, limit, true
		}
		return limit, 0, true
	}
	rm.instanceTypes = map[string]*ec2.InstanceTypeInfo{"t3.micro": micro}
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	defer nc.Close()
	rm.initSubscriptions(nc, func(*nats.Msg) string { return outcomeSuccess }, nil, "vb-node")
	queueSubs, nodeSubs := countSubsBySuffix(rm.instanceSubs, ".vb-node")
	require.Equal(t, 1, queueSubs)
	require.Equal(t, 1, nodeSubs)

	full.Store(true)
	rm.updateInstanceSubscriptions()
	queueSubs, nodeSubs = countSubsBySuffix(rm.instanceSubs, ".vb-node")
	assert.Equal(t, 0, queueSubs, "a full node leaves the queue")
	assert.Equal(t, 1, nodeSubs, "the node-targeted subject stays, and allocate refuses there")
	require.Error(t, rm.allocate(micro))

	full.Store(false)
	rm.updateInstanceSubscriptions()
	queueSubs, _ = countSubsBySuffix(rm.instanceSubs, ".vb-node")
	assert.Equal(t, 1, queueSubs, "the node rejoins the queue when the reading falls")
}

func TestViperblockGateNeverRefusesAnAdoptedGuest(t *testing.T) {
	rm, micro := gatedRM(t, 0)
	require.NoError(t, rm.allocateAdopted(micro), "a guest restored on startup is counted, not refused")
	assert.Equal(t, 21, rm.exports)
}

func TestViperblockGateReopensWhenAVolumeCloses(t *testing.T) {
	rm, micro := gatedRM(t, 0)
	require.Equal(t, 0, rm.CanAllocate(micro, 1))
	rm.deallocate(micro)
	assert.Equal(t, 1, rm.CanAllocate(micro, 1), "a terminate frees a slot")

	// Reclaim alone: memory.current falls with no terminate, and the next read admits.
	rm2, _ := gatedRM(t, 20)
	full := true
	limit, _, _ := rm2.readViperblockMem()
	rm2.readViperblockMem = func() (int64, int64, bool) {
		if full {
			return limit, limit, true
		}
		return limit, 0, true
	}
	require.Equal(t, 0, rm2.CanAllocate(micro, 1))
	full = false
	assert.Equal(t, 1, rm2.CanAllocate(micro, 1))
}

func TestViperblockGateRefusesAReservationLaunchAtTheLimit(t *testing.T) {
	rm, micro := gatedRM(t, 0)
	rm.reservations["cr-1"] = &capacityReservation{AccountID: "111122223333", InstanceType: "t3.micro",
		TotalInstanceCount: 1, VCPUPerInstance: 2, MemGBPerInstance: 1.75}
	err := rm.AllocateFromReservation("cr-1", "111122223333", micro)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, err.Error())
}

func TestViperblockGateIsOpenWithoutACgroup(t *testing.T) {
	rm, micro := gatedRM(t, 0)
	rm.readViperblockMem = func() (int64, int64, bool) { return 0, 0, false }
	assert.Equal(t, 1, rm.CanAllocate(micro, 1), "a node with no viperblock cgroup has no export limit")
}

// StartInstances on a node whose viperblock limit is full answers InsufficientInstanceCapacity,
// as AWS does for a start it cannot place, and the instance stays stopped for a retry.
func TestStartInstancesRefusedWhenViperblockMemoryIsFull(t *testing.T) {
	t.Parallel()
	store := vmmock.New()
	v := stoppedVMFixture("i-vbfull-start", testAccountID)
	store.Stopped[v.ID] = v
	d := daemonWithFakeStateStore(t, store)
	d.resourceMgr.mu.Lock()
	d.resourceMgr.readViperblockMem = func() (int64, int64, bool) { return 10 * 768, 10 * 768, true }
	d.resourceMgr.mu.Unlock()

	body, _ := json.Marshal(ec2instance.StartStoppedInstanceInput{InstanceID: v.ID})
	reply := requestHandler(t, d.natsConn, "ec2.start.vbfull", asMsgHandler(d.handleEC2StartStoppedInstance), testAccountID, body)
	assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, decodeError(t, reply.Data)["Code"])
	restored, ok := store.WroteStopped[v.ID]
	require.True(t, ok, "a refused start writes the claimed instance back to the stopped store")
	assert.Equal(t, vm.StateStopped, restored.Status, "it stays stopped, with its volume, for a retry")
}

// The scheduler places launches from the node's census, so a full node must report no
// room there, or it keeps being chosen and refuses each launch at allocate.
func TestViperblockGateClampsTheCensusTheSchedulerReads(t *testing.T) {
	rm, micro := gatedRM(t, 0)
	rm.instanceTypes = map[string]*ec2.InstanceTypeInfo{"t3.micro": micro}
	_, _, _, _, _, _, caps := rm.GetResourceStats()
	for _, c := range caps {
		assert.Zero(t, c.Available, "a full node advertises no %s", c.Name)
	}
	assert.Empty(t, rm.GetInstanceTypeAvailability(), "and no availability")

	rm.deallocate(micro)
	_, _, _, _, _, _, caps = rm.GetResourceStats()
	require.NotEmpty(t, caps)
	assert.Equal(t, 1, caps[0].Available, "a freed slot is advertised again")
}
