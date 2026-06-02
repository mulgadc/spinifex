package gpu

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnapshot_EmptyManager(t *testing.T) {
	m := NewManager(nil)
	assert.Empty(t, m.Snapshot())
}

func TestSnapshot_WholeGPUPool(t *testing.T) {
	devA := GPUDevice{PCIAddress: "0000:01:00.0", Model: "NVIDIA A10", MemoryMiB: 23028}
	devB := GPUDevice{PCIAddress: "0000:02:00.0", Model: "NVIDIA A10", MemoryMiB: 23028}
	m := NewManager([]GPUDevice{devA, devB})

	snap := m.Snapshot()
	require.Len(t, snap, 2)

	assert.Equal(t, "0000:01:00.0", snap[0].Device.PCIAddress)
	assert.Nil(t, snap[0].MIGInstance)
	assert.Empty(t, snap[0].InstanceID)
	assert.True(t, snap[0].Available)

	assert.Equal(t, "0000:02:00.0", snap[1].Device.PCIAddress)
}

func TestSnapshot_WholeGPUClaimed(t *testing.T) {
	dev := GPUDevice{
		PCIAddress: "0000:01:00.0",
		Model:      "NVIDIA A10",
		MemoryMiB:  23028,
		IOMMUGroup: -1, // skip IOMMU enumeration in tests
	}
	m := NewManager([]GPUDevice{dev})
	// Directly mutate pool to simulate a claimed entry without VFIO sysfs calls.
	m.pool[0].InstanceID = "i-abc123"

	snap := m.Snapshot()
	require.Len(t, snap, 1)
	assert.Equal(t, "i-abc123", snap[0].InstanceID)
	assert.Nil(t, snap[0].MIGInstance)
}

func TestSnapshot_MIGSlices(t *testing.T) {
	root := t.TempDir()
	mdev1 := makeMdevPath(t, root, "uuid-gi1")
	mdev2 := makeMdevPath(t, root, "uuid-gi2")

	dev := newMIGDevice("0000:01:00.0")
	m := NewManager(nil)
	m.AddMIGInstances(dev, []MIGInstance{
		{GIID: 1, MdevPath: mdev1, Profile: MIGProfile{Name: "1g.24gb", MemoryMiB: 24576}},
		{GIID: 2, MdevPath: mdev2, Profile: MIGProfile{Name: "1g.24gb", MemoryMiB: 24576}},
	})
	m.pool[0].InstanceID = "i-aaa"

	snap := m.Snapshot()
	require.Len(t, snap, 2)

	assert.Equal(t, "0000:01:00.0", snap[0].Device.PCIAddress)
	require.NotNil(t, snap[0].MIGInstance)
	assert.Equal(t, 1, snap[0].MIGInstance.GIID)
	assert.Equal(t, "1g.24gb", snap[0].MIGInstance.Profile.Name)
	assert.Equal(t, int64(24576), snap[0].MIGInstance.Profile.MemoryMiB)
	assert.Equal(t, "i-aaa", snap[0].InstanceID)

	assert.Empty(t, snap[1].InstanceID)
}

func TestSnapshot_FreeMIGGPUsIncluded(t *testing.T) {
	dev := newMIGDevice("0000:03:00.0")
	m := NewManager(nil)
	m.AddMIGGPU(dev)

	snap := m.Snapshot()
	require.Len(t, snap, 1)
	assert.Equal(t, "0000:03:00.0", snap[0].Device.PCIAddress)
	assert.Nil(t, snap[0].MIGInstance)
	assert.True(t, snap[0].Available)
	assert.Empty(t, snap[0].InstanceID)
}

func TestSnapshot_IsolatedFromPool(t *testing.T) {
	dev := GPUDevice{PCIAddress: "0000:01:00.0", Model: "NVIDIA A10", MemoryMiB: 23028}
	m := NewManager([]GPUDevice{dev})

	snap := m.Snapshot()
	require.Len(t, snap, 1)

	// Mutating the snapshot must not affect the manager pool.
	snap[0].InstanceID = "i-injected"
	snap[0].Device.Model = "tampered"

	snap2 := m.Snapshot()
	assert.Empty(t, snap2[0].InstanceID)
	assert.Equal(t, "NVIDIA A10", snap2[0].Device.Model)
}

func TestSnapshot_MIGInstanceIsolated(t *testing.T) {
	root := t.TempDir()
	mdev := makeMdevPath(t, root, "uuid-gi1")
	dev := newMIGDevice("0000:01:00.0")
	m := NewManager(nil)
	m.AddMIGInstances(dev, []MIGInstance{
		{GIID: 1, MdevPath: mdev, Profile: MIGProfile{Name: "1g.24gb", MemoryMiB: 24576}},
	})

	snap := m.Snapshot()
	require.Len(t, snap, 1)
	require.NotNil(t, snap[0].MIGInstance)

	// Mutating the returned MIGInstance must not affect the pool.
	snap[0].MIGInstance.Profile.Name = "tampered"

	snap2 := m.Snapshot()
	assert.Equal(t, "1g.24gb", snap2[0].MIGInstance.Profile.Name)
}

func makeMdevPath(t *testing.T, root, uuid string) string {
	t.Helper()
	p := filepath.Join(root, uuid)
	require.NoError(t, os.MkdirAll(p, 0o755))
	return p
}
