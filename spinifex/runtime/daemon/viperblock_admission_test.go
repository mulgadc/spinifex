package daemon

import (
	"sync"
	"sync/atomic"
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
	// A 37.65 GiB limit less its 5% margin is 36,627 MiB: 47 root volumes at 768 MiB.
	limit := int64(251 * 1024 * 15 / 100)
	assert.Equal(t, 47, exportCount(100, limit, 0, 0, 768), "an empty node fits 47")
	assert.Equal(t, 5, exportCount(5, limit, 0, 0, 768), "a request under the limit is not clamped")
	assert.Equal(t, 7, exportCount(100, limit, 0, 40, 768), "admitted exports count before they open")
	assert.Equal(t, 0, exportCount(100, limit, limit, 0, 768), "a full cgroup admits none")
	// Three attached data volumes show in memory.current, so the next launch refuses sooner.
	assert.Equal(t, 4, exportCount(100, limit, 43*768, 40, 768), "memory.current counts attached volumes")
	assert.Equal(t, 0, exportCount(0, limit, 0, 0, 768))
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
