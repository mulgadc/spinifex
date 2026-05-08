package gpu

import (
	"os"
	"path/filepath"
	"testing"
)

// newManagerForTest creates a Manager backed by a custom sysfsRoot for unit tests.
func newManagerForTest(devices []GPUDevice, sysfsRoot string) *Manager {
	m := NewManager(devices)
	m.sysfsRoot = sysfsRoot
	return m
}

// buildManagerSysfs sets up a complete sysfs mock for a single GPU with a
// companion audio device in the same IOMMU group. Creates all driver
// directories needed for bind/unbind to succeed.
func buildManagerSysfs(t *testing.T) (root string, gpu GPUDevice) {
	t.Helper()
	root = t.TempDir()

	buildSysfsDevice(t, root, "0000:03:00.0", "0x030200", "0x10de", "0x2236", "nvidia", 7)
	buildSysfsDevice(t, root, "0000:03:00.1", "0x040300", "0x10de", "0x1aef", "snd_hda_intel", 7)

	makeSysfsDriverDir(t, root, "vfio-pci")
	makeSysfsDriverDir(t, root, "nvidia")
	makeSysfsDriverDir(t, root, "snd_hda_intel")

	gpu = GPUDevice{
		PCIAddress:     "0000:03:00.0",
		Vendor:         VendorNVIDIA,
		VendorID:       "10de",
		DeviceID:       "2236",
		Model:          "NVIDIA A10",
		MemoryMiB:      23028,
		IOMMUGroup:     7,
		OriginalDriver: "nvidia",
	}
	return root, gpu
}

func TestManagerClaim_Success(t *testing.T) {
	root, gpu := buildManagerSysfs(t)
	m := newManagerForTest([]GPUDevice{gpu}, root)

	if m.Available() != 1 {
		t.Fatalf("want Available=1 before claim, got %d", m.Available())
	}

	dev, err := m.Claim("i-001")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if dev.PCIAddress != gpu.PCIAddress {
		t.Errorf("claimed device PCI = %q, want %q", dev.PCIAddress, gpu.PCIAddress)
	}
	if m.Available() != 0 {
		t.Errorf("want Available=0 after claim, got %d", m.Available())
	}
	if m.AllocatedCount() != 1 {
		t.Errorf("want AllocatedCount=1 after claim, got %d", m.AllocatedCount())
	}
	if m.TotalCount() != 1 {
		t.Errorf("want TotalCount=1, got %d", m.TotalCount())
	}
}

func TestManagerClaim_NoGPUAvailable(t *testing.T) {
	root, gpu := buildManagerSysfs(t)
	m := newManagerForTest([]GPUDevice{gpu}, root)

	if _, err := m.Claim("i-001"); err != nil {
		t.Fatalf("first Claim: %v", err)
	}

	// Second claim on a pool with only one GPU.
	_, err := m.Claim("i-002")
	if err == nil {
		t.Error("want error when no GPU available, got nil")
	}
}

func TestManagerClaim_NoIOMMU(t *testing.T) {
	root := t.TempDir()
	gpu := GPUDevice{PCIAddress: "0000:03:00.0", IOMMUGroup: -1}
	m := newManagerForTest([]GPUDevice{gpu}, root)

	_, err := m.Claim("i-001")
	if err == nil {
		t.Error("want error for GPU with no IOMMU group, got nil")
	}
}

func TestManagerRelease_Success(t *testing.T) {
	root, gpu := buildManagerSysfs(t)
	m := newManagerForTest([]GPUDevice{gpu}, root)

	if _, err := m.Claim("i-001"); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	if err := m.Release("i-001"); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if m.Available() != 1 {
		t.Errorf("want Available=1 after release, got %d", m.Available())
	}
	if m.AllocatedCount() != 0 {
		t.Errorf("want AllocatedCount=0 after release, got %d", m.AllocatedCount())
	}
}

func TestManagerRelease_UnknownInstance(t *testing.T) {
	root, gpu := buildManagerSysfs(t)
	m := newManagerForTest([]GPUDevice{gpu}, root)

	err := m.Release("i-does-not-exist")
	if err == nil {
		t.Error("want error releasing unknown instance, got nil")
	}
}

func TestManagerRelease_FailureMarksUnavailable(t *testing.T) {
	root, gpu := buildManagerSysfs(t)
	m := newManagerForTest([]GPUDevice{gpu}, root)

	if _, err := m.Claim("i-001"); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// Simulate what the kernel does after vfio-pci binding: update the driver
	// symlink for each IOMMU group member to point to vfio-pci.
	vfioDriverPath := filepath.Join(root, "bus/pci/drivers/vfio-pci")
	for _, addr := range []string{"0000:03:00.0", "0000:03:00.1"} {
		devPath := filepath.Join(root, "bus/pci/devices", addr)
		_ = os.Remove(filepath.Join(devPath, "driver"))
		must(t, os.Symlink(vfioDriverPath, filepath.Join(devPath, "driver")))
	}

	// Make the vfio-pci driver directory non-writable so the unbind write fails.
	vfioDir := filepath.Join(root, "bus/pci/drivers/vfio-pci")
	must(t, os.Chmod(vfioDir, 0o555))
	t.Cleanup(func() { os.Chmod(vfioDir, 0o755) })

	err := m.Release("i-001")
	if err == nil {
		t.Error("want error when vfio-pci unbind fails, got nil")
	}

	// Pool entry must be marked unavailable so the broken GPU isn't re-offered.
	if m.Available() != 0 {
		t.Errorf("want Available=0 after failed release, got %d", m.Available())
	}
	if m.TotalCount() != 1 {
		t.Errorf("TotalCount should still reflect physical GPU count, got %d", m.TotalCount())
	}
}

func TestManagerClaim_RollbackOnPartialFailure(t *testing.T) {
	root := t.TempDir()

	buildSysfsDevice(t, root, "0000:03:00.0", "0x030200", "0x10de", "0x2236", "nvidia", 7)
	buildSysfsDevice(t, root, "0000:03:00.1", "0x040300", "0x10de", "0x1aef", "snd_hda_intel", 7)

	makeSysfsDriverDir(t, root, "vfio-pci")
	makeSysfsDriverDir(t, root, "nvidia")
	makeSysfsDriverDir(t, root, "snd_hda_intel")

	// Pre-create the audio device's driver_override as read-only so the bind
	// sequence fails when it tries to write "vfio-pci" into it.
	audioDevPath := filepath.Join(root, "bus/pci/devices/0000:03:00.1")
	must(t, os.WriteFile(filepath.Join(audioDevPath, "driver_override"), []byte(""), 0o444))

	gpu := GPUDevice{
		PCIAddress: "0000:03:00.0", Vendor: VendorNVIDIA,
		VendorID: "10de", DeviceID: "2236", IOMMUGroup: 7, OriginalDriver: "nvidia",
	}
	m := newManagerForTest([]GPUDevice{gpu}, root)

	_, err := m.Claim("i-001")
	if err == nil {
		t.Error("want error when partial IOMMU group bind fails, got nil")
	}

	// GPU must be back in the pool after rollback.
	if m.Available() != 1 {
		t.Errorf("want Available=1 after rollback, got %d", m.Available())
	}
	if m.AllocatedCount() != 0 {
		t.Errorf("want AllocatedCount=0 after rollback, got %d", m.AllocatedCount())
	}
}

func TestManagerClaim_GroupMembersError(t *testing.T) {
	root := t.TempDir()
	// IOMMU group 99 has no devices directory — groupMembers will fail.
	gpu := GPUDevice{PCIAddress: "0000:03:00.0", IOMMUGroup: 99, OriginalDriver: "nvidia"}
	m := newManagerForTest([]GPUDevice{gpu}, root)

	_, err := m.Claim("i-001")
	if err == nil {
		t.Error("want error when IOMMU group directory is missing, got nil")
	}
}

// TestManagerRelease_PreBound verifies that when a GPU was already bound to
// vfio-pci before the daemon started (OriginalDriver == "vfio-pci"), Release
// skips the sysfs unbind/rebind and just returns the device to the pool.
// This is the normal path for nodes set up with spx-test-gpu.
func TestManagerRelease_PreBound(t *testing.T) {
	root := t.TempDir()

	// GPU already bound to vfio-pci (simulating spx-test-gpu setup).
	buildSysfsDevice(t, root, "0000:03:00.0", "0x030200", "0x10de", "0x2487", "vfio-pci", 14)
	buildSysfsDevice(t, root, "0000:03:00.1", "0x040300", "0x10de", "0x228b", "vfio-pci", 14)
	makeSysfsDriverDir(t, root, "vfio-pci")

	gpu := GPUDevice{
		PCIAddress:     "0000:03:00.0",
		Vendor:         VendorNVIDIA,
		VendorID:       "10de",
		DeviceID:       "2487",
		IOMMUGroup:     14,
		OriginalDriver: "vfio-pci",
	}
	m := newManagerForTest([]GPUDevice{gpu}, root)

	if _, err := m.Claim("i-001"); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// Make vfio-pci driver dir read-only to prove Release doesn't write to it.
	vfioDir := filepath.Join(root, "bus/pci/drivers/vfio-pci")
	must(t, os.Chmod(vfioDir, 0o555))
	t.Cleanup(func() { os.Chmod(vfioDir, 0o755) })

	if err := m.Release("i-001"); err != nil {
		t.Fatalf("Release on pre-bound GPU should not write sysfs, got err: %v", err)
	}

	if m.Available() != 1 {
		t.Errorf("want Available=1 after release, got %d", m.Available())
	}
	if m.AllocatedCount() != 0 {
		t.Errorf("want AllocatedCount=0 after release, got %d", m.AllocatedCount())
	}
}

func TestManagerCounts_MultiGPU(t *testing.T) {
	root := t.TempDir()

	for i, addr := range []string{"0000:03:00.0", "0000:04:00.0"} {
		buildSysfsDevice(t, root, addr, "0x030200", "0x10de", "0x2236", "nvidia", i+7)
		makeSysfsDriverDir(t, root, "nvidia")
		makeSysfsDriverDir(t, root, "vfio-pci")
	}

	gpus := []GPUDevice{
		{PCIAddress: "0000:03:00.0", IOMMUGroup: 7, OriginalDriver: "nvidia"},
		{PCIAddress: "0000:04:00.0", IOMMUGroup: 8, OriginalDriver: "nvidia"},
	}
	m := newManagerForTest(gpus, root)

	if m.TotalCount() != 2 {
		t.Fatalf("want TotalCount=2, got %d", m.TotalCount())
	}

	if _, err := m.Claim("i-001"); err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	if m.Available() != 1 || m.AllocatedCount() != 1 {
		t.Errorf("after first claim: Available=%d AllocatedCount=%d, want 1/1",
			m.Available(), m.AllocatedCount())
	}

	if _, err := m.Claim("i-002"); err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if m.Available() != 0 || m.AllocatedCount() != 2 {
		t.Errorf("after second claim: Available=%d AllocatedCount=%d, want 0/2",
			m.Available(), m.AllocatedCount())
	}

	if err := m.Release("i-001"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if m.Available() != 1 || m.AllocatedCount() != 1 {
		t.Errorf("after release: Available=%d AllocatedCount=%d, want 1/1",
			m.Available(), m.AllocatedCount())
	}
}

func TestManagerReclaim_Success(t *testing.T) {
	root, gpu := buildManagerSysfs(t)
	m := newManagerForTest([]GPUDevice{gpu}, root)

	if err := m.ReclaimByAddress(gpu.PCIAddress, "i-001"); err != nil {
		t.Fatalf("ReclaimByAddress: %v", err)
	}
	if m.AllocatedCount() != 1 {
		t.Errorf("want AllocatedCount=1 after reclaim, got %d", m.AllocatedCount())
	}
	if m.Available() != 0 {
		t.Errorf("want Available=0 after reclaim, got %d", m.Available())
	}
}

func TestManagerReclaim_AlreadyClaimed(t *testing.T) {
	root, gpu := buildManagerSysfs(t)
	m := newManagerForTest([]GPUDevice{gpu}, root)

	if _, err := m.Claim("i-001"); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	err := m.ReclaimByAddress(gpu.PCIAddress, "i-002")
	if err == nil {
		t.Error("want error when GPU already claimed, got nil")
	}
}

// OnInstanceUp can fire on a freshly-launched instance whose handler-side
// Claim already populated the slot with the same instance ID. Reclaim must
// short-circuit instead of returning an "already claimed" error so callers
// don't log a spurious warning on every successful Run/Start.
func TestManagerReclaim_SameInstanceIsNoop(t *testing.T) {
	root, gpu := buildManagerSysfs(t)
	m := newManagerForTest([]GPUDevice{gpu}, root)

	if _, err := m.Claim("i-001"); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	if err := m.ReclaimByAddress(gpu.PCIAddress, "i-001"); err != nil {
		t.Fatalf("ReclaimByAddress for same instance: got %v, want nil", err)
	}
	if m.AllocatedCount() != 1 {
		t.Errorf("AllocatedCount after same-id reclaim: got %d, want 1", m.AllocatedCount())
	}
}

func TestManagerReclaim_NotFound(t *testing.T) {
	root, gpu := buildManagerSysfs(t)
	m := newManagerForTest([]GPUDevice{gpu}, root)

	err := m.ReclaimByAddress("0000:ff:00.0", "i-001")
	if err == nil {
		t.Error("want error for unknown PCI address, got nil")
	}
}

func TestManagerReclaim_GroupMembersError(t *testing.T) {
	root := t.TempDir()
	// GPU with IOMMU group 99 but no group directory — groupMembers falls back to single member.
	buildSysfsDevice(t, root, "0000:03:00.0", "0x030200", "0x10de", "0x2236", "vfio-pci", -1)
	gpu := GPUDevice{
		PCIAddress:     "0000:03:00.0",
		Vendor:         VendorNVIDIA,
		VendorID:       "10de",
		DeviceID:       "2236",
		IOMMUGroup:     99,
		OriginalDriver: "nvidia",
	}
	m := newManagerForTest([]GPUDevice{gpu}, root)

	// Must succeed even though IOMMU group can't be read (fallback to single member).
	if err := m.ReclaimByAddress(gpu.PCIAddress, "i-001"); err != nil {
		t.Fatalf("ReclaimByAddress with missing IOMMU group: %v", err)
	}
	if m.AllocatedCount() != 1 {
		t.Errorf("want AllocatedCount=1, got %d", m.AllocatedCount())
	}
}
