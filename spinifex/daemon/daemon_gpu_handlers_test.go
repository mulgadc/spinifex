package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/gpu"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gpuStatusRequest subscribes and requests handleNodeStatus on a fresh subject
// so each test gets an isolated round trip.
func gpuStatusRequest(t *testing.T, daemon *Daemon, subject string) types.NodeStatusResponse {
	t.Helper()
	sub, err := daemon.natsConn.Subscribe(subject, daemon.handleNodeStatus)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reply, err := daemon.natsConn.Request(subject, nil, 5*time.Second)
	require.NoError(t, err)

	var resp types.NodeStatusResponse
	require.NoError(t, json.Unmarshal(reply.Data, &resp))
	return resp
}

// gpuVMsRequest subscribes and requests handleNodeVMs on a fresh subject.
func gpuVMsRequest(t *testing.T, daemon *Daemon, subject string) types.NodeVMsResponse {
	t.Helper()
	sub, err := daemon.natsConn.Subscribe(subject, daemon.handleNodeVMs)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	reply, err := daemon.natsConn.Request(subject, nil, 5*time.Second)
	require.NoError(t, err)

	var resp types.NodeVMsResponse
	require.NoError(t, json.Unmarshal(reply.Data, &resp))
	return resp
}

func TestHandleNodeStatus_NoGPU(t *testing.T) {
	daemon := createTestDaemon(t, sharedNATSURL)
	daemon.gpuManager = nil

	resp := gpuStatusRequest(t, daemon, "spinifex.node.status.nogpu")

	assert.Empty(t, resp.GPUs)
	assert.False(t, resp.GPUPassthrough)
	assert.Equal(t, 0, resp.TotalGPUs)
}

func TestHandleNodeStatus_WholeGPU(t *testing.T) {
	daemon := createTestDaemon(t, sharedNATSURL)

	claimed := gpu.GPUDevice{PCIAddress: "0000:01:00.0", Model: "NVIDIA A10", MemoryMiB: 23028, IOMMUGroup: -1}
	free := gpu.GPUDevice{PCIAddress: "0000:02:00.0", Model: "NVIDIA A10", MemoryMiB: 23028, IOMMUGroup: -1}
	mgr := gpu.NewManager([]gpu.GPUDevice{claimed, free})
	require.NoError(t, mgr.ReclaimByAddress(claimed.PCIAddress, "i-claimer"))
	daemon.gpuManager = mgr

	resp := gpuStatusRequest(t, daemon, "spinifex.node.status.wholegpu")

	require.Len(t, resp.GPUs, 2)
	byPCI := make(map[string]types.GPUInfo, 2)
	for _, g := range resp.GPUs {
		byPCI[g.PCIAddress] = g
	}

	claimedInfo := byPCI[claimed.PCIAddress]
	assert.Equal(t, "NVIDIA A10", claimedInfo.Model)
	assert.Equal(t, int64(23028), claimedInfo.VRAMMiB)
	assert.False(t, claimedInfo.MIGEnabled)
	assert.Empty(t, claimedInfo.MIGProfile)
	assert.Nil(t, claimedInfo.Slices)
	assert.Equal(t, "i-claimer", claimedInfo.InstanceID)

	freeInfo := byPCI[free.PCIAddress]
	assert.Empty(t, freeInfo.InstanceID)
	assert.Nil(t, freeInfo.Slices)
}

func TestHandleNodeStatus_MIG(t *testing.T) {
	daemon := createTestDaemon(t, sharedNATSURL)

	mdevDir := t.TempDir()
	mdev1 := filepath.Join(mdevDir, "uuid-gi1")
	mdev2 := filepath.Join(mdevDir, "uuid-gi2")
	require.NoError(t, os.MkdirAll(mdev1, 0o755))
	require.NoError(t, os.MkdirAll(mdev2, 0o755))

	dev := gpu.GPUDevice{
		PCIAddress: "0000:03:00.0",
		Model:      "NVIDIA RTX Pro 6000 Blackwell SE",
		MemoryMiB:  98304,
		IOMMUGroup: -1,
		MIGCapable: true,
		MIGEnabled: true,
	}
	mgr := gpu.NewManager(nil)
	mgr.AddMIGInstances(dev, []gpu.MIGInstance{
		{GIID: 1, MdevPath: mdev1, Profile: gpu.MIGProfile{Name: "1g.24gb", MemoryMiB: 24576}},
		{GIID: 2, MdevPath: mdev2, Profile: gpu.MIGProfile{Name: "1g.24gb", MemoryMiB: 24576}},
	})
	require.NoError(t, mgr.ReclaimByMdev(mdev1, "i-slice-owner"))
	daemon.gpuManager = mgr

	resp := gpuStatusRequest(t, daemon, "spinifex.node.status.mig")

	require.Len(t, resp.GPUs, 1)
	g := resp.GPUs[0]
	assert.Equal(t, dev.PCIAddress, g.PCIAddress)
	assert.Equal(t, dev.Model, g.Model)
	assert.True(t, g.MIGEnabled)
	assert.Equal(t, "1g.24gb", g.MIGProfile)
	assert.Empty(t, g.InstanceID, "whole-GPU InstanceID is unused in MIG mode")
	require.Len(t, g.Slices, 2)

	byGI := make(map[int]types.GPUSliceInfo, 2)
	for _, s := range g.Slices {
		byGI[s.GIID] = s
	}
	assert.Equal(t, "i-slice-owner", byGI[1].InstanceID)
	assert.Equal(t, "1g.24gb", byGI[1].Profile)
	assert.Equal(t, int64(24576), byGI[1].VRAMMiB)
	assert.Equal(t, mdev1, byGI[1].MdevPath)
	assert.Empty(t, byGI[2].InstanceID, "unclaimed slice is free")
}

func TestHandleNodeVMs_GPUAttachment_WholeGPU(t *testing.T) {
	daemon := createTestDaemon(t, sharedNATSURL)
	instanceType := getTestInstanceType(t)

	dev := gpu.GPUDevice{PCIAddress: "0000:01:00.0", Model: "NVIDIA A10", MemoryMiB: 23028, IOMMUGroup: -1}
	mgr := gpu.NewManager([]gpu.GPUDevice{dev})
	require.NoError(t, mgr.ReclaimByAddress(dev.PCIAddress, "i-gpu-vm"))
	daemon.gpuManager = mgr

	daemon.vmMgr.Insert(&vm.VM{
		ID:             "i-gpu-vm",
		Status:         vm.StateRunning,
		InstanceType:   instanceType,
		GPUAttachments: []gpu.GPUAttachment{{PCIAddress: dev.PCIAddress}},
	})

	resp := gpuVMsRequest(t, daemon, "spinifex.node.vms.gpuwhole")

	require.Len(t, resp.VMs, 1)
	require.NotNil(t, resp.VMs[0].GPU)
	assert.Equal(t, "NVIDIA A10", resp.VMs[0].GPU.Model)
	assert.Equal(t, int64(23028), resp.VMs[0].GPU.VRAMMiB)
	assert.Equal(t, dev.PCIAddress, resp.VMs[0].GPU.PCIAddress)
	assert.Empty(t, resp.VMs[0].GPU.Profile)
	assert.Empty(t, resp.VMs[0].GPU.MdevPath)
}

func TestHandleNodeVMs_GPUAttachment_MIG(t *testing.T) {
	daemon := createTestDaemon(t, sharedNATSURL)
	instanceType := getTestInstanceType(t)

	mdevDir := t.TempDir()
	mdevPath := filepath.Join(mdevDir, "uuid-gi1")
	require.NoError(t, os.MkdirAll(mdevPath, 0o755))

	dev := gpu.GPUDevice{PCIAddress: "0000:03:00.0", Model: "NVIDIA RTX Pro 6000 Blackwell SE", MemoryMiB: 98304}
	mgr := gpu.NewManager(nil)
	mgr.AddMIGInstances(dev, []gpu.MIGInstance{
		{GIID: 1, MdevPath: mdevPath, Profile: gpu.MIGProfile{Name: "1g.24gb", MemoryMiB: 24576}},
	})
	require.NoError(t, mgr.ReclaimByMdev(mdevPath, "i-mig-vm"))
	daemon.gpuManager = mgr

	daemon.vmMgr.Insert(&vm.VM{
		ID:             "i-mig-vm",
		Status:         vm.StateRunning,
		InstanceType:   instanceType,
		GPUAttachments: []gpu.GPUAttachment{{MdevPath: mdevPath}},
	})

	resp := gpuVMsRequest(t, daemon, "spinifex.node.vms.gpumig")

	require.Len(t, resp.VMs, 1)
	require.NotNil(t, resp.VMs[0].GPU)
	assert.Equal(t, dev.Model, resp.VMs[0].GPU.Model)
	assert.Equal(t, int64(24576), resp.VMs[0].GPU.VRAMMiB)
	assert.Equal(t, "1g.24gb", resp.VMs[0].GPU.Profile)
	assert.Equal(t, mdevPath, resp.VMs[0].GPU.MdevPath)
	assert.Empty(t, resp.VMs[0].GPU.PCIAddress)
}

func TestHandleNodeVMs_GPUAttachment_Absent(t *testing.T) {
	daemon := createTestDaemon(t, sharedNATSURL)
	instanceType := getTestInstanceType(t)

	daemon.vmMgr.Insert(&vm.VM{
		ID:           "i-no-gpu",
		Status:       vm.StateRunning,
		InstanceType: instanceType,
	})

	resp := gpuVMsRequest(t, daemon, "spinifex.node.vms.nogpu")

	require.Len(t, resp.VMs, 1)
	assert.Nil(t, resp.VMs[0].GPU)
}
