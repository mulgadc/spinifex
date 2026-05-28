package daemon

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/gpu"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- buildGPUInventory ---

func TestBuildGPUInventory_Empty(t *testing.T) {
	assert.Empty(t, buildGPUInventory(nil))
	assert.Empty(t, buildGPUInventory([]gpu.PoolEntry{}))
}

func TestBuildGPUInventory_WholeGPU_Free(t *testing.T) {
	snap := []gpu.PoolEntry{
		{
			Device:    gpu.GPUDevice{PCIAddress: "0000:01:00.0", Model: "NVIDIA A10", MemoryMiB: 23028},
			Available: true,
		},
	}
	gpus := buildGPUInventory(snap)
	require.Len(t, gpus, 1)
	assert.Equal(t, "0000:01:00.0", gpus[0].PCIAddress)
	assert.Equal(t, "NVIDIA A10", gpus[0].Model)
	assert.Equal(t, int64(23028), gpus[0].VRAMMiB)
	assert.False(t, gpus[0].MIGEnabled)
	assert.Empty(t, gpus[0].InstanceID)
	assert.Nil(t, gpus[0].Slices)
}

func TestBuildGPUInventory_WholeGPU_Claimed(t *testing.T) {
	snap := []gpu.PoolEntry{
		{
			Device:     gpu.GPUDevice{PCIAddress: "0000:01:00.0", Model: "NVIDIA A10", MemoryMiB: 23028},
			InstanceID: "i-abc123",
			Available:  true,
		},
	}
	gpus := buildGPUInventory(snap)
	require.Len(t, gpus, 1)
	assert.Equal(t, "i-abc123", gpus[0].InstanceID)
}

func TestBuildGPUInventory_MIG_FourSlices(t *testing.T) {
	dev := gpu.GPUDevice{
		PCIAddress: "0000:01:00.0",
		Model:      "NVIDIA RTX Pro 6000 Blackwell Server Edition",
		MemoryMiB:  98304,
	}
	profile := gpu.MIGProfile{Name: "1g.24gb", MemoryMiB: 24576}
	snap := []gpu.PoolEntry{
		{Device: dev, MIGInstance: &gpu.MIGInstance{GIID: 1, MdevPath: "/dev/mdev/uuid-1", Profile: profile}, InstanceID: "i-aaa", Available: true},
		{Device: dev, MIGInstance: &gpu.MIGInstance{GIID: 2, MdevPath: "/dev/mdev/uuid-2", Profile: profile}, InstanceID: "i-bbb", Available: true},
		{Device: dev, MIGInstance: &gpu.MIGInstance{GIID: 3, MdevPath: "/dev/mdev/uuid-3", Profile: profile}, InstanceID: "i-ccc", Available: true},
		{Device: dev, MIGInstance: &gpu.MIGInstance{GIID: 4, MdevPath: "/dev/mdev/uuid-4", Profile: profile}, InstanceID: "", Available: true},
	}

	gpus := buildGPUInventory(snap)
	require.Len(t, gpus, 1, "four slices of one physical GPU should produce one GPUInfo")

	g := gpus[0]
	assert.Equal(t, "0000:01:00.0", g.PCIAddress)
	assert.True(t, g.MIGEnabled)
	assert.Equal(t, "1g.24gb", g.MIGProfile)
	assert.Empty(t, g.InstanceID, "InstanceID is for whole-GPU only")
	require.Len(t, g.Slices, 4)

	assert.Equal(t, 1, g.Slices[0].GIID)
	assert.Equal(t, "i-aaa", g.Slices[0].InstanceID)
	assert.Equal(t, "1g.24gb", g.Slices[0].Profile)
	assert.Equal(t, int64(24576), g.Slices[0].VRAMMiB)
	assert.Equal(t, "/dev/mdev/uuid-1", g.Slices[0].MdevPath)

	assert.Empty(t, g.Slices[3].InstanceID, "last slice is free")
}

func TestBuildGPUInventory_MultiplePhysicalGPUs(t *testing.T) {
	dev1 := gpu.GPUDevice{PCIAddress: "0000:01:00.0", Model: "NVIDIA A10", MemoryMiB: 23028}
	dev2 := gpu.GPUDevice{PCIAddress: "0000:02:00.0", Model: "NVIDIA A10", MemoryMiB: 23028}
	snap := []gpu.PoolEntry{
		{Device: dev1, InstanceID: "i-aaa", Available: true},
		{Device: dev2, Available: true},
	}
	gpus := buildGPUInventory(snap)
	require.Len(t, gpus, 2)
	assert.Equal(t, "0000:01:00.0", gpus[0].PCIAddress)
	assert.Equal(t, "0000:02:00.0", gpus[1].PCIAddress)
}

func TestBuildGPUInventory_PreservesOrder(t *testing.T) {
	addresses := []string{"0000:03:00.0", "0000:01:00.0", "0000:02:00.0"}
	var snap []gpu.PoolEntry
	for _, addr := range addresses {
		snap = append(snap, gpu.PoolEntry{
			Device:    gpu.GPUDevice{PCIAddress: addr},
			Available: true,
		})
	}
	gpus := buildGPUInventory(snap)
	require.Len(t, gpus, 3)
	for i, addr := range addresses {
		assert.Equal(t, addr, gpus[i].PCIAddress)
	}
}

// --- buildPoolLookup ---

func TestBuildPoolLookup_NilManager(t *testing.T) {
	byMdev, byPCI := buildPoolLookup(nil)
	assert.Nil(t, byMdev)
	assert.Nil(t, byPCI)
}

// --- resolveVMGPU ---

func TestResolveVMGPU_MIGAttachment(t *testing.T) {
	profile := gpu.MIGProfile{Name: "1g.24gb", MemoryMiB: 24576}
	dev := gpu.GPUDevice{PCIAddress: "0000:01:00.0", Model: "NVIDIA RTX Pro 6000 Blackwell Server Edition", MemoryMiB: 98304}
	entry := gpu.PoolEntry{
		Device:      dev,
		MIGInstance: &gpu.MIGInstance{GIID: 1, MdevPath: "/dev/mdev/uuid-1", Profile: profile},
		InstanceID:  "i-abc",
	}
	byMdev := map[string]gpu.PoolEntry{"/dev/mdev/uuid-1": entry}

	att := gpu.GPUAttachment{MdevPath: "/dev/mdev/uuid-1"}
	result := resolveVMGPU(att, byMdev, nil)
	require.NotNil(t, result)
	assert.Equal(t, "NVIDIA RTX Pro 6000 Blackwell Server Edition", result.Model)
	assert.Equal(t, int64(24576), result.VRAMMiB)
	assert.Equal(t, "1g.24gb", result.Profile)
	assert.Equal(t, "/dev/mdev/uuid-1", result.MdevPath)
}

func TestResolveVMGPU_WholeGPUAttachment(t *testing.T) {
	dev := gpu.GPUDevice{PCIAddress: "0000:01:00.0", Model: "NVIDIA A10", MemoryMiB: 23028}
	entry := gpu.PoolEntry{Device: dev, InstanceID: "i-abc"}
	byPCI := map[string]gpu.PoolEntry{"0000:01:00.0": entry}

	att := gpu.GPUAttachment{PCIAddress: "0000:01:00.0"}
	result := resolveVMGPU(att, nil, byPCI)
	require.NotNil(t, result)
	assert.Equal(t, "NVIDIA A10", result.Model)
	assert.Equal(t, int64(23028), result.VRAMMiB)
	assert.Empty(t, result.Profile)
	assert.Empty(t, result.MdevPath)
}

func TestResolveVMGPU_UnknownMdev_ReturnsNil(t *testing.T) {
	att := gpu.GPUAttachment{MdevPath: "/dev/mdev/unknown"}
	assert.Nil(t, resolveVMGPU(att, map[string]gpu.PoolEntry{}, nil))
}

func TestResolveVMGPU_UnknownPCI_ReturnsNil(t *testing.T) {
	att := gpu.GPUAttachment{PCIAddress: "0000:99:00.0"}
	assert.Nil(t, resolveVMGPU(att, nil, map[string]gpu.PoolEntry{}))
}

func TestResolveVMGPU_EmptyAttachment_ReturnsNil(t *testing.T) {
	assert.Nil(t, resolveVMGPU(gpu.GPUAttachment{}, nil, nil))
}

// --- types shape: ensure GPU fields are present ---

func TestVMGPUInfo_JSONFields(t *testing.T) {
	info := types.VMGPUInfo{
		Model:   "NVIDIA A10",
		VRAMMiB: 23028,
	}
	assert.Equal(t, "NVIDIA A10", info.Model)
	assert.Equal(t, int64(23028), info.VRAMMiB)
	assert.Empty(t, info.Profile)
	assert.Empty(t, info.MdevPath)
}

func TestGPUInfo_SlicesNilForWholeGPU(t *testing.T) {
	info := types.GPUInfo{PCIAddress: "0000:01:00.0"}
	assert.Equal(t, "0000:01:00.0", info.PCIAddress)
	assert.False(t, info.MIGEnabled)
	assert.Nil(t, info.Slices)
}
