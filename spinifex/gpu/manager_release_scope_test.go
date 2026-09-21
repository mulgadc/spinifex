// asserts on unexported pool state that no exported accessor distinguishes.
//
//test:in-package — drives Release against the in-package sysfs fixtures and
package gpu

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A multi-GPU instance releasing with one bad device must lose only that one.
// firstErr used to be scoped to the whole release, so the first failure
// condemned every GPU processed after it — unreachable while an instance held
// one GPU, and a node-emptying event once it could hold eight.
func TestManagerRelease_FailureIsScopedToTheFailingGPU(t *testing.T) {
	root := t.TempDir()
	buildSysfsDevice(t, root, "0000:03:00.0", "0x030200", "0x10de", "0x2236", "nvidia", 7)
	buildSysfsDevice(t, root, "0000:04:00.0", "0x030200", "0x10de", "0x2236", "nvidia", 8)
	makeSysfsDriverDir(t, root, "vfio-pci")
	makeSysfsDriverDir(t, root, "nvidia")

	m := newManagerForTest([]GPUDevice{
		{PCIAddress: "0000:03:00.0", Vendor: VendorNVIDIA, VendorID: "10de", DeviceID: "2236",
			IOMMUGroup: 7, OriginalDriver: "nvidia"},
		{PCIAddress: "0000:04:00.0", Vendor: VendorNVIDIA, VendorID: "10de", DeviceID: "2236",
			IOMMUGroup: 8, OriginalDriver: "nvidia"},
	}, root)

	for range 2 {
		_, _, err := m.Claim("i-001", "")
		require.NoError(t, err)
	}
	require.Zero(t, m.AvailableWhole(), "both GPUs are claimed")

	// The fixture does not move the driver symlink the way the kernel does on
	// bind, and Release skips a device that is not vfio-pci bound.
	vfioDriverPath := filepath.Join(root, "bus/pci/drivers/vfio-pci")
	for _, addr := range []string{"0000:03:00.0", "0000:04:00.0"} {
		devPath := filepath.Join(root, "bus/pci/devices", addr)
		require.NoError(t, os.Remove(filepath.Join(devPath, "driver")))
		require.NoError(t, os.Symlink(vfioDriverPath, filepath.Join(devPath, "driver")))
	}

	// Break only the first GPU's unbind, by way of its own driver_override.
	// A directory in the file's place fails the write whatever the test uid,
	// where a read-only mode would not stop root.
	override := filepath.Join(root, "bus/pci/devices/0000:03:00.0/driver_override")
	require.NoError(t, os.Remove(override))
	require.NoError(t, os.Mkdir(override, 0o755))

	err := m.Release("i-001")
	require.Error(t, err, "a failed unbind is still reported")

	assert.Equal(t, 1, m.AvailableWhole(),
		"the GPU that unbound cleanly must return to the pool")
	assert.Equal(t, 2, m.TotalCount(), "both GPUs are still physically present")

	for _, e := range m.pool {
		switch e.Device.PCIAddress {
		case "0000:03:00.0":
			assert.False(t, e.Available, "the GPU that failed to unbind is held back")
		case "0000:04:00.0":
			assert.True(t, e.Available, "its sibling was never touched by the failure")
		}
		assert.Empty(t, e.InstanceID, "every entry is unbound from the instance either way")
	}
}
