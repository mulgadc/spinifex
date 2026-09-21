//test:in-package — drives ResourceManager.canAllocateLocked and builds
package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/gpu"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gpuTypeForTest builds an InstanceTypeInfo whose cpu/mem fit the test node
// comfortably, so only the GPU gate can bind the admission count.
func gpuTypeForTest(name string, gpus int64) *ec2.InstanceTypeInfo {
	return &ec2.InstanceTypeInfo{
		InstanceType: aws.String(name),
		VCpuInfo:     &ec2.VCpuInfo{DefaultVCpus: aws.Int64(2)},
		MemoryInfo:   &ec2.MemoryInfo{SizeInMiB: aws.Int64(1024)},
		GpuInfo: &ec2.GpuInfo{
			Gpus: []*ec2.GpuDeviceInfo{{
				Count:        aws.Int64(gpus),
				Manufacturer: aws.String("NVIDIA"),
				Name:         aws.String("A10G"),
				MemoryInfo:   &ec2.GpuDeviceMemoryInfo{SizeInMiB: aws.Int64(24576)},
			}},
			TotalGpuMemoryInMiB: aws.Int64(24576 * gpus),
		},
	}
}

// carvedMIGManager returns a manager holding only carved MIG slices — no whole
// GPU is free.
func carvedMIGManager(t *testing.T, slices int) *gpu.Manager {
	t.Helper()
	root := t.TempDir()
	dev := gpu.GPUDevice{
		PCIAddress: "0000:01:00.0", VendorID: "10de", DeviceID: "20b5",
		MIGCapable: true, MIGEnabled: true, IOMMUGroup: -1,
	}
	instances := make([]gpu.MIGInstance, slices)
	for i := range instances {
		path := filepath.Join(root, string(rune('a'+i)))
		require.NoError(t, os.MkdirAll(path, 0755))
		instances[i] = gpu.MIGInstance{
			GIID:     i + 1,
			MdevPath: path,
			Profile:  gpu.MIGProfile{Name: "1g.10gb", MemoryMiB: 10240},
		}
	}
	m := gpu.NewManager(nil)
	m.AddMIGInstances(dev, instances)
	return m
}

// A node with free slices and no free whole GPU must refuse a whole-GPU type at
// admission, not fail it later at Claim.
func TestAdmission_FreeSlicesDoNotBackAWholeGPUType(t *testing.T) {
	rm := &ResourceManager{
		hostVCPU:   64,
		hostMemGB:  256.0,
		gpuManager: carvedMIGManager(t, 7),
	}

	assert.Zero(t, rm.canAllocateLocked(gpuTypeForTest("g5.xlarge", 1), 10),
		"seven slices are not one whole GPU")
	assert.Equal(t, 7, rm.canAllocateLocked(gpuTypeForTest("mig.1g.10gb", 1), 10),
		"the same seven slices back seven MIG instances")
	assert.Zero(t, rm.canAllocateLocked(gpuTypeForTest("mig.3g.40gb", 1), 10),
		"a profile with no slices carved is not available")
}

// A multi-GPU type divides the free whole GPUs by its own count.
func TestAdmission_MultiGPUTypeDividesTheWholePool(t *testing.T) {
	devices := make([]gpu.GPUDevice, 8)
	for i := range devices {
		devices[i] = gpu.GPUDevice{PCIAddress: string(rune('a' + i)), IOMMUGroup: i}
	}
	rm := &ResourceManager{
		hostVCPU:   256,
		hostMemGB:  1024.0,
		gpuManager: gpu.NewManager(devices),
	}

	assert.Equal(t, 8, rm.canAllocateLocked(gpuTypeForTest("gpu.1x4c", 1), 10))
	assert.Equal(t, 2, rm.canAllocateLocked(gpuTypeForTest("gpu.4x4c", 4), 10))
	assert.Equal(t, 1, rm.canAllocateLocked(gpuTypeForTest("g5.48xlarge", 8), 10))
}

// A reservation is a counter over vCPU and memory slots and reserves no pool
// entry, so its free slots say nothing about whether a GPU is there to claim.
func TestReservationAvailable_GatesOnFreeGPUs(t *testing.T) {
	gpuType := gpuTypeForTest("gpu.4x4c", 4)
	devices := make([]gpu.GPUDevice, 4)
	for i := range devices {
		devices[i] = gpu.GPUDevice{PCIAddress: string(rune('a' + i)), IOMMUGroup: i}
	}
	mgr := gpu.NewManager(devices)

	rm := &ResourceManager{
		hostVCPU:      64,
		hostMemGB:     256.0,
		gpuManager:    mgr,
		instanceTypes: map[string]*ec2.InstanceTypeInfo{"gpu.4x4c": gpuType},
		reservations:  make(map[string]*capacityReservation),
	}

	// Three slots reserved, but only four GPUs on the node — one instance.
	require.NoError(t, rm.CreateReservation(&capacityReservation{
		ID:                    "cr-gpu",
		AccountID:             "acct-a",
		InstanceType:          "gpu.4x4c",
		AvailabilityZone:      "ap-southeast-2a",
		TotalInstanceCount:    3,
		VCPUPerInstance:       2,
		MemGBPerInstance:      1.0,
		InstanceMatchCriteria: "open",
		Tenancy:               "default",
		InstancePlatform:      "Linux/UNIX",
		CreateDate:            time.Now(),
	}))

	assert.Equal(t, 1, rm.ReservationAvailable("cr-gpu", "acct-a", gpuType),
		"three reserved slots cannot outrun four GPUs at four per instance")

	// With the GPUs gone from the pool the reservation backs nothing at all.
	for i := range devices {
		mgr.MarkFailed(devices[i].PCIAddress)
	}
	assert.Zero(t, rm.ReservationAvailable("cr-gpu", "acct-a", gpuType))
}

// ReservationAvailable answers under the read lock and reserves nothing, so two
// targeted launches can both clear it. AllocateFromReservation re-checks under
// the write lock, as the general path does — otherwise the loser consumes a
// reservation slot and then dies at claim time with no GPU to take.
func TestAllocateFromReservation_RechecksFreeGPUs(t *testing.T) {
	gpuType := gpuTypeForTest("gpu.4x4c", 4)
	devices := make([]gpu.GPUDevice, 4)
	for i := range devices {
		devices[i] = gpu.GPUDevice{PCIAddress: string(rune('a' + i)), IOMMUGroup: i}
	}
	mgr := gpu.NewManager(devices)

	rm := &ResourceManager{
		hostVCPU:      64,
		hostMemGB:     256.0,
		gpuManager:    mgr,
		instanceTypes: map[string]*ec2.InstanceTypeInfo{"gpu.4x4c": gpuType},
		reservations:  make(map[string]*capacityReservation),
	}
	require.NoError(t, rm.CreateReservation(&capacityReservation{
		ID:                    "cr-gpu",
		AccountID:             "acct-a",
		InstanceType:          "gpu.4x4c",
		AvailabilityZone:      "ap-southeast-2a",
		TotalInstanceCount:    3,
		VCPUPerInstance:       2,
		MemGBPerInstance:      1.0,
		InstanceMatchCriteria: "open",
		Tenancy:               "default",
		InstancePlatform:      "Linux/UNIX",
		CreateDate:            time.Now(),
	}))

	require.NoError(t, rm.AllocateFromReservation("cr-gpu", "acct-a", gpuType),
		"the first launch takes the node's only four GPUs")

	// The GPUs leave the pool between the two calls — claimed by the winner of
	// the race, failed, or dropped by a SIGHUP rebuild. Two slots still remain.
	for i := range devices {
		mgr.MarkFailed(devices[i].PCIAddress)
	}

	err := rm.AllocateFromReservation("cr-gpu", "acct-a", gpuType)
	require.Error(t, err, "free slots do not conjure a GPU")
	assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, err.Error(),
		"the reservation is not what ran out")
}

// A non-GPU reservation keeps answering on slots alone.
func TestReservationAvailable_NonGPUTypeIsUngated(t *testing.T) {
	rm := newReservationTestRM()
	rm.gpuManager = gpu.NewManager(nil)
	require.NoError(t, rm.CreateReservation(microReservation("cr-1", "acct-a", 2)))

	assert.Equal(t, 2, rm.ReservationAvailable("cr-1", "acct-a", microType()))
}
