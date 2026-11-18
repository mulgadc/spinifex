package gpu

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

	dev, _, err := m.Claim("i-001", "")
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

// TestManagerClaim_SkipsBridgeGroupMember reproduces a host topology where a
// GPU shares its IOMMU group with an upstream PCIe bridge (no ACS isolation
// between them). vfio-pci refuses to bind bridge-class devices, so Claim must
// skip the bridge entirely rather than fail the whole claim, and Release must
// leave the bridge on its original driver afterward.
func TestManagerClaim_SkipsBridgeGroupMember(t *testing.T) {
	useKernfs(t)
	root, gpu := buildManagerSysfs(t)
	// Upstream PCIe bridge in the same IOMMU group as the GPU (0x0604 = PCI-to-PCI bridge).
	buildSysfsDevice(t, root, "0000:00:1c.0", "0x060400", "0x8086", "0x0db0", "pcieport", 7)
	makeSysfsDriverDir(t, root, "pcieport")

	m := newManagerForTest([]GPUDevice{gpu}, root)

	if _, _, err := m.Claim("i-001", ""); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	bridgeDriver, err := os.Readlink(filepath.Join(root, "bus/pci/devices/0000:00:1c.0/driver"))
	if err != nil {
		t.Fatalf("read bridge driver link: %v", err)
	}
	if filepath.Base(bridgeDriver) != "pcieport" {
		t.Errorf("bridge driver = %q, want pcieport (must not be rebound to vfio-pci)", filepath.Base(bridgeDriver))
	}

	if err := m.Release("i-001"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if m.Available() != 1 {
		t.Errorf("want Available=1 after release, got %d", m.Available())
	}
}

func TestManagerClaim_NoGPUAvailable(t *testing.T) {
	root, gpu := buildManagerSysfs(t)
	m := newManagerForTest([]GPUDevice{gpu}, root)

	if _, _, err := m.Claim("i-001", ""); err != nil {
		t.Fatalf("first Claim: %v", err)
	}

	// Second claim on a pool with only one GPU.
	_, _, err := m.Claim("i-002", "")
	if err == nil {
		t.Error("want error when no GPU available, got nil")
	}
}

func TestManagerClaim_NoIOMMU(t *testing.T) {
	root := t.TempDir()
	gpu := GPUDevice{PCIAddress: "0000:03:00.0", IOMMUGroup: -1}
	m := newManagerForTest([]GPUDevice{gpu}, root)

	_, _, err := m.Claim("i-001", "")
	if err == nil {
		t.Error("want error for GPU with no IOMMU group, got nil")
	}
}

func TestManagerRelease_Success(t *testing.T) {
	useKernfs(t)
	root, gpu := buildManagerSysfs(t)
	m := newManagerForTest([]GPUDevice{gpu}, root)

	if _, _, err := m.Claim("i-001", ""); err != nil {
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
		t.Fatal("want error releasing unknown instance, got nil")
	}
	// Matchable rather than a bare message: teardown re-drives releases and has
	// to tell "already released" from a rebind failure without reading text.
	if !errors.Is(err, ErrNoGPUClaimed) {
		t.Errorf("want ErrNoGPUClaimed, got %v", err)
	}
	if !strings.Contains(err.Error(), "i-does-not-exist") {
		t.Errorf("want the instance ID in the message, got %q", err)
	}
}

func TestManagerRelease_FailureMarksUnavailable(t *testing.T) {
	root, gpu := buildManagerSysfs(t)
	m := newManagerForTest([]GPUDevice{gpu}, root)

	if _, _, err := m.Claim("i-001", ""); err != nil {
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
	// Must stay distinct from the benign already-released case, which callers
	// are entitled to ignore. This one clears Available, the health signal the
	// Bedrock capacity gate reads.
	if errors.Is(err, ErrNoGPUClaimed) {
		t.Errorf("a rebind failure must not report as ErrNoGPUClaimed, got %v", err)
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
	useKernfs(t)
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

	_, _, err := m.Claim("i-001", "")
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

	_, _, err := m.Claim("i-001", "")
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

	if _, _, err := m.Claim("i-001", ""); err != nil {
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
	useKernfs(t)
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

	if _, _, err := m.Claim("i-001", ""); err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	if m.Available() != 1 || m.AllocatedCount() != 1 {
		t.Errorf("after first claim: Available=%d AllocatedCount=%d, want 1/1",
			m.Available(), m.AllocatedCount())
	}

	if _, _, err := m.Claim("i-002", ""); err != nil {
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

func TestManagerMarkFailed_MakesUnavailable(t *testing.T) {
	root, gpu := buildManagerSysfs(t)
	m := newManagerForTest([]GPUDevice{gpu}, root)

	if m.Available() != 1 {
		t.Fatalf("want Available=1 before MarkFailed, got %d", m.Available())
	}
	m.MarkFailed(gpu.PCIAddress)
	if m.Available() != 0 {
		t.Errorf("want Available=0 after MarkFailed, got %d", m.Available())
	}
	if m.TotalCount() != 1 {
		t.Errorf("want TotalCount=1 after MarkFailed (entry retained), got %d", m.TotalCount())
	}
	// MarkFailed on an unknown address must be a no-op (not panic).
	m.MarkFailed("ffff:ff:ff.f")
}

func TestManagerMarkFailed_ClaimedEntryIgnored(t *testing.T) {
	root, gpu := buildManagerSysfs(t)
	m := newManagerForTest([]GPUDevice{gpu}, root)

	if _, _, err := m.Claim("i-running", ""); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// MarkFailed on a claimed (non-empty InstanceID) entry must not mark it unavailable.
	m.MarkFailed(gpu.PCIAddress)
	if m.AllocatedCount() != 1 {
		t.Errorf("claimed GPU must still be allocated after MarkFailed, AllocatedCount=%d", m.AllocatedCount())
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

	if _, _, err := m.Claim("i-001", ""); err != nil {
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

	if _, _, err := m.Claim("i-001", ""); err != nil {
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

// assertBinding checks a function's bound driver and its driver_override.
func assertBinding(t *testing.T, root, addr, driver, override string) {
	t.Helper()
	devPath := filepath.Join(root, "bus/pci/devices", addr)
	if got, err := readDriver(devPath); err != nil || got != driver {
		t.Errorf("%s driver = %q (err %v), want %q", addr, got, err, driver)
	}
	if got := readSysfsTestFile(t, filepath.Join(devPath, "driver_override")); got != override+"\n" {
		t.Errorf("%s driver_override = %q, want %q", addr, got, override)
	}
}

func TestManagerRelease_RestoresGroupDriversForReclaim(t *testing.T) {
	useKernfs(t)
	root, gpu := buildManagerSysfs(t)
	m := newManagerForTest([]GPUDevice{gpu}, root)

	if _, _, err := m.Claim("i-001", ""); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	assertBinding(t, root, "0000:03:00.0", "vfio-pci", "vfio-pci")
	assertBinding(t, root, "0000:03:00.1", "vfio-pci", "vfio-pci")

	if err := m.Release("i-001"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	assertBinding(t, root, "0000:03:00.0", "nvidia", "(null)")
	assertBinding(t, root, "0000:03:00.1", "snd_hda_intel", "(null)")
	if m.Available() != 1 {
		t.Fatalf("want Available=1 after release, got %d", m.Available())
	}
	if _, _, err := m.Claim("i-002", ""); err != nil {
		t.Fatalf("re-Claim after release: %v", err)
	}
	assertBinding(t, root, "0000:03:00.1", "vfio-pci", "vfio-pci")
}

func TestManagerRelease_FailedClearHoldsGPU(t *testing.T) {
	useKernfs(t)
	root, gpu := buildManagerSysfs(t)
	m := newManagerForTest([]GPUDevice{gpu}, root)
	if _, _, err := m.Claim("i-001", ""); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// A directory in the attribute's place fails the clear whatever the uid.
	override := filepath.Join(root, "bus/pci/devices/0000:03:00.1/driver_override")
	must(t, os.Remove(override))
	must(t, os.Mkdir(override, 0o755))

	if err := m.Release("i-001"); err == nil {
		t.Fatal("want Release error when the override cannot be cleared")
	}
	if m.Available() != 0 {
		t.Errorf("want Available=0 after failed clear, got %d", m.Available())
	}
	if _, err := os.Stat(filepath.Join(root, "bus/pci/drivers/snd_hda_intel/bind")); !os.IsNotExist(err) {
		t.Errorf("snd_hda_intel/bind must not be written after a failed clear (stat err %v)", err)
	}
}

// buildRollbackSysfs builds group 7 with a GPU, a second function bound to
// second, and an audio function whose driver's unbind attribute is a
// directory, so binding it fails before anything about it changes.
func buildRollbackSysfs(t *testing.T, gpuDriver, second string) (string, GPUDevice) {
	t.Helper()
	root := t.TempDir()
	buildSysfsDevice(t, root, "0000:03:00.0", "0x030200", "0x10de", "0x2236", gpuDriver, 7)
	buildSysfsDevice(t, root, "0000:03:00.1", "0x0c0330", "0x10de", "0x1ad6", second, 7)
	buildSysfsDevice(t, root, "0000:03:00.2", "0x040300", "0x10de", "0x1aef", "snd_hda_intel", 7)
	for _, d := range []string{"vfio-pci", "nvidia", "xhci_hcd", "snd_hda_intel"} {
		makeSysfsDriverDir(t, root, d)
	}
	for _, addr := range []string{"0000:03:00.0", "0000:03:00.1", "0000:03:00.2"} {
		ov := "(null)"
		if driver, _ := readDriver(filepath.Join(root, "bus/pci/devices", addr)); driver == "vfio-pci" {
			ov = "vfio-pci"
		}
		must(t, os.WriteFile(filepath.Join(root, "bus/pci/devices", addr, "driver_override"), []byte(ov+"\n"), 0o600))
	}
	must(t, os.Mkdir(filepath.Join(root, "bus/pci/drivers/snd_hda_intel/unbind"), 0o755))
	return root, GPUDevice{
		PCIAddress: "0000:03:00.0", Vendor: VendorNVIDIA,
		VendorID: "10de", DeviceID: "2236", IOMMUGroup: 7, OriginalDriver: "nvidia",
	}
}

func TestManagerClaim_RollbackLeavesPreBoundPrimary(t *testing.T) {
	useKernfs(t)
	// Discovery saw nvidia, but the GPU was already on vfio-pci at claim time.
	root, gpu := buildRollbackSysfs(t, "vfio-pci", "xhci_hcd")
	m := newManagerForTest([]GPUDevice{gpu}, root)

	if _, _, err := m.Claim("i-001", ""); err == nil {
		t.Fatal("want Claim error when a later member fails to bind")
	}
	assertBinding(t, root, "0000:03:00.0", "vfio-pci", "vfio-pci")
	assertBinding(t, root, "0000:03:00.1", "xhci_hcd", "(null)")
	assertBinding(t, root, "0000:03:00.2", "snd_hda_intel", "(null)")
	if m.Available() != 1 {
		t.Errorf("want Available=1 after a clean rollback, got %d", m.Available())
	}
}

func TestManagerClaim_RollbackLeavesPreBoundCompanion(t *testing.T) {
	useKernfs(t)
	root, gpu := buildRollbackSysfs(t, "nvidia", "vfio-pci")
	m := newManagerForTest([]GPUDevice{gpu}, root)

	if _, _, err := m.Claim("i-001", ""); err == nil {
		t.Fatal("want Claim error when a later member fails to bind")
	}
	assertBinding(t, root, "0000:03:00.0", "nvidia", "(null)")
	assertBinding(t, root, "0000:03:00.1", "vfio-pci", "vfio-pci")
	assertBinding(t, root, "0000:03:00.2", "snd_hda_intel", "(null)")
	if m.Available() != 1 {
		t.Errorf("want Available=1 after a clean rollback, got %d", m.Available())
	}
}

func TestManagerClaim_FailedRollbackHoldsGPU(t *testing.T) {
	useKernfs(t)
	root, gpu := buildRollbackSysfs(t, "nvidia", "xhci_hcd")
	// Restoring the GPU to nvidia fails, so the rollback cannot complete.
	must(t, os.Mkdir(filepath.Join(root, "bus/pci/drivers/nvidia/bind"), 0o755))
	m := newManagerForTest([]GPUDevice{gpu}, root)

	if _, _, err := m.Claim("i-001", ""); err == nil {
		t.Fatal("want Claim error when a later member fails to bind")
	}
	if m.Available() != 0 {
		t.Errorf("want Available=0 after a failed rollback, got %d", m.Available())
	}
	if m.AllocatedCount() != 0 {
		t.Errorf("want AllocatedCount=0 after a failed claim, got %d", m.AllocatedCount())
	}
}

// Release must not report success for a function already moved off vfio-pci
// unless it is back on its original driver with no override.
func TestManagerRelease_ChecksFunctionsAlreadyOffVFIO(t *testing.T) {
	useKernfs(t)
	const audio = "0000:03:00.1"
	unbindVFIOExternally := func(t *testing.T, root string) {
		must(t, kernfsWrite(filepath.Join(root, "bus/pci/drivers/vfio-pci/unbind"), audio))
	}
	clearOverride := func(t *testing.T, root string) {
		must(t, kernfsWrite(filepath.Join(root, "bus/pci/devices", audio, "driver_override"), "\n"))
	}
	cases := []struct {
		name      string
		origAudio string
		mutate    func(t *testing.T, root string)
		wantErr   bool
	}{
		{"stale override", "snd_hda_intel", unbindVFIOExternally, true},
		{"originally unbound with stale override", "", unbindVFIOExternally, true},
		{"missing device", "snd_hda_intel", func(t *testing.T, root string) {
			must(t, os.RemoveAll(filepath.Join(root, "bus/pci/devices", audio)))
		}, true},
		{"bound to another driver", "snd_hda_intel", func(t *testing.T, root string) {
			unbindVFIOExternally(t, root)
			clearOverride(t, root)
			makeSysfsDriverDir(t, root, "other")
			must(t, kernfsWrite(filepath.Join(root, "bus/pci/drivers/other/bind"), audio))
		}, true},
		{"originally unbound and cleared", "", func(t *testing.T, root string) {
			unbindVFIOExternally(t, root)
			clearOverride(t, root)
		}, false},
		{"whitespace override on the original driver", "snd_hda_intel", func(t *testing.T, root string) {
			unbindVFIOExternally(t, root)
			clearOverride(t, root)
			must(t, kernfsWrite(filepath.Join(root, "bus/pci/drivers/snd_hda_intel/bind"), audio))
			must(t, kernfsWrite(filepath.Join(root, "bus/pci/devices", audio, "driver_override"), " "))
		}, true},
		{"unreadable override on the original driver", "snd_hda_intel", func(t *testing.T, root string) {
			unbindVFIOExternally(t, root)
			clearOverride(t, root)
			must(t, kernfsWrite(filepath.Join(root, "bus/pci/drivers/snd_hda_intel/bind"), audio))
			override := filepath.Join(root, "bus/pci/devices", audio, "driver_override")
			must(t, os.Remove(override))
			must(t, os.Mkdir(override, 0o755))
		}, true},
		{"already restored", "snd_hda_intel", func(t *testing.T, root string) {
			unbindVFIOExternally(t, root)
			clearOverride(t, root)
			must(t, kernfsWrite(filepath.Join(root, "bus/pci/drivers/snd_hda_intel/bind"), audio))
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			buildSysfsDevice(t, root, "0000:03:00.0", "0x030200", "0x10de", "0x2236", "nvidia", 7)
			buildSysfsDevice(t, root, audio, "0x040300", "0x10de", "0x1aef", tc.origAudio, 7)
			for _, d := range []string{"vfio-pci", "nvidia", "snd_hda_intel"} {
				makeSysfsDriverDir(t, root, d)
			}
			gpu := GPUDevice{
				PCIAddress: "0000:03:00.0", Vendor: VendorNVIDIA,
				VendorID: "10de", DeviceID: "2236", IOMMUGroup: 7, OriginalDriver: "nvidia",
			}
			m := newManagerForTest([]GPUDevice{gpu}, root)
			if _, _, err := m.Claim("i-001", ""); err != nil {
				t.Fatalf("Claim: %v", err)
			}
			tc.mutate(t, root)

			err := m.Release("i-001")
			if tc.wantErr {
				if err == nil {
					t.Fatal("want Release error")
				}
				if m.Available() != 0 {
					t.Errorf("want Available=0, got %d", m.Available())
				}
				return
			}
			if err != nil {
				t.Fatalf("Release: %v", err)
			}
			if m.Available() != 1 {
				t.Errorf("want Available=1, got %d", m.Available())
			}
		})
	}
}

// A function pre-bound to vfio-pci is left as found on release, but only while
// it is still there; otherwise the GPU is held without touching the function.
func TestManagerRelease_ChecksPreBoundMember(t *testing.T) {
	useKernfs(t)
	const audio = "0000:03:00.1"
	cases := []struct {
		name       string
		mutate     func(t *testing.T, root string)
		wantErr    bool
		wantDriver string
	}{
		{"still on vfio-pci", func(t *testing.T, root string) {}, false, "vfio-pci"},
		{"unbound", func(t *testing.T, root string) {
			must(t, kernfsWrite(filepath.Join(root, "bus/pci/drivers/vfio-pci/unbind"), audio))
		}, true, ""},
		{"bound to another driver", func(t *testing.T, root string) {
			must(t, kernfsWrite(filepath.Join(root, "bus/pci/drivers/vfio-pci/unbind"), audio))
			must(t, kernfsWrite(filepath.Join(root, "bus/pci/devices", audio, "driver_override"), "\n"))
			must(t, kernfsWrite(filepath.Join(root, "bus/pci/drivers/snd_hda_intel/bind"), audio))
		}, true, "snd_hda_intel"},
		{"missing device", func(t *testing.T, root string) {
			must(t, os.RemoveAll(filepath.Join(root, "bus/pci/devices", audio)))
		}, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, gpu := buildManagerSysfs(t)
			devPath := filepath.Join(root, "bus/pci/devices", audio)
			must(t, kernfsWrite(filepath.Join(root, "bus/pci/drivers/snd_hda_intel/unbind"), audio))
			must(t, kernfsWrite(filepath.Join(devPath, "driver_override"), "vfio-pci"))
			must(t, kernfsWrite(filepath.Join(root, "bus/pci/drivers/vfio-pci/bind"), audio))
			m := newManagerForTest([]GPUDevice{gpu}, root)
			if _, _, err := m.Claim("i-001", ""); err != nil {
				t.Fatalf("Claim: %v", err)
			}
			tc.mutate(t, root)
			var before string
			if _, err := os.Stat(devPath); err == nil {
				before = readSysfsTestFile(t, filepath.Join(devPath, "driver_override"))
			}

			err := m.Release("i-001")
			assertBinding(t, root, "0000:03:00.0", "nvidia", "(null)")
			if tc.wantErr != (err != nil) {
				t.Fatalf("Release err = %v, wantErr %v", err, tc.wantErr)
			}
			if want := map[bool]int{true: 0, false: 1}[tc.wantErr]; m.Available() != want {
				t.Errorf("want Available=%d, got %d", want, m.Available())
			}
			if before != "" {
				assertBinding(t, root, audio, tc.wantDriver, strings.TrimSuffix(before, "\n"))
			}
		})
	}
}

// preBindGPU moves the primary onto vfio-pci after discovery recorded nvidia.
func preBindGPU(t *testing.T, root string) {
	t.Helper()
	const gpuAddr = "0000:03:00.0"
	must(t, kernfsWrite(filepath.Join(root, "bus/pci/drivers/nvidia/unbind"), gpuAddr))
	must(t, kernfsWrite(filepath.Join(root, "bus/pci/devices", gpuAddr, "driver_override"), "vfio-pci"))
	must(t, kernfsWrite(filepath.Join(root, "bus/pci/drivers/vfio-pci/bind"), gpuAddr))
}

func TestManagerRollbackClaim_LeavesClaimTimePreBoundGPU(t *testing.T) {
	useKernfs(t)
	root, gpu := buildManagerSysfs(t)
	preBindGPU(t, root)
	m := newManagerForTest([]GPUDevice{gpu}, root)
	if _, _, err := m.Claim("i-001", ""); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// A two-GPU request on a one-GPU pool: the second claim fails, as the claimer sees it.
	if _, _, err := m.Claim("i-001", ""); err == nil {
		t.Fatal("want the second Claim to fail on a one-GPU pool")
	}

	if err := m.RollbackClaim("i-001"); err != nil {
		t.Fatalf("RollbackClaim: %v", err)
	}
	assertBinding(t, root, "0000:03:00.0", "vfio-pci", "vfio-pci")
	assertBinding(t, root, "0000:03:00.1", "snd_hda_intel", "(null)")
	if m.Available() != 1 {
		t.Errorf("want Available=1, got %d", m.Available())
	}
}

func TestManagerRollbackClaim_RestoresClaimMovedGPU(t *testing.T) {
	useKernfs(t)
	root, gpu := buildManagerSysfs(t)
	m := newManagerForTest([]GPUDevice{gpu}, root)
	if _, _, err := m.Claim("i-001", ""); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// A two-GPU request on a one-GPU pool: the second claim fails, as the claimer sees it.
	if _, _, err := m.Claim("i-001", ""); err == nil {
		t.Fatal("want the second Claim to fail on a one-GPU pool")
	}

	if err := m.RollbackClaim("i-001"); err != nil {
		t.Fatalf("RollbackClaim: %v", err)
	}
	assertBinding(t, root, "0000:03:00.0", "nvidia", "(null)")
	assertBinding(t, root, "0000:03:00.1", "snd_hda_intel", "(null)")
	if m.Available() != 1 {
		t.Errorf("want Available=1, got %d", m.Available())
	}
}

// Normal release still returns a claim-time pre-bound primary to its discovery driver.
func TestManagerRelease_PreBoundPrimaryReturnsToDiscoveryDriver(t *testing.T) {
	useKernfs(t)
	root, gpu := buildManagerSysfs(t)
	preBindGPU(t, root)
	m := newManagerForTest([]GPUDevice{gpu}, root)
	if _, _, err := m.Claim("i-001", ""); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	if err := m.Release("i-001"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	assertBinding(t, root, "0000:03:00.0", "nvidia", "(null)")
}

// A companion re-claimed after a restart has no recorded original driver.
func TestManagerRelease_ReclaimedCompanionWithUnknownOriginal(t *testing.T) {
	useKernfs(t)
	const audio = "0000:03:00.1"
	cases := []struct {
		name       string
		mutate     func(t *testing.T, root string)
		wantErr    bool
		wantDriver string
	}{
		{"still on vfio-pci is unbound and cleared", func(t *testing.T, root string) {}, false, ""},
		{"already unbound", func(t *testing.T, root string) {
			must(t, kernfsWrite(filepath.Join(root, "bus/pci/drivers/vfio-pci/unbind"), audio))
			must(t, kernfsWrite(filepath.Join(root, "bus/pci/devices", audio, "driver_override"), "\n"))
		}, true, ""},
		{"already on another driver", func(t *testing.T, root string) {
			must(t, kernfsWrite(filepath.Join(root, "bus/pci/drivers/vfio-pci/unbind"), audio))
			must(t, kernfsWrite(filepath.Join(root, "bus/pci/devices", audio, "driver_override"), "\n"))
			must(t, kernfsWrite(filepath.Join(root, "bus/pci/drivers/snd_hda_intel/bind"), audio))
		}, true, "snd_hda_intel"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, gpu := buildManagerSysfs(t)
			// A live instance survived the restart, so discovery saw vfio-pci.
			preBindGPU(t, root)
			must(t, kernfsWrite(filepath.Join(root, "bus/pci/drivers/snd_hda_intel/unbind"), audio))
			must(t, kernfsWrite(filepath.Join(root, "bus/pci/devices", audio, "driver_override"), "vfio-pci"))
			must(t, kernfsWrite(filepath.Join(root, "bus/pci/drivers/vfio-pci/bind"), audio))
			gpu.OriginalDriver = "vfio-pci"
			m := newManagerForTest([]GPUDevice{gpu}, root)
			if err := m.ReclaimByAddress(gpu.PCIAddress, "i-001"); err != nil {
				t.Fatalf("ReclaimByAddress: %v", err)
			}
			tc.mutate(t, root)

			err := m.Release("i-001")
			if tc.wantErr != (err != nil) {
				t.Fatalf("Release err = %v, wantErr %v", err, tc.wantErr)
			}
			if want := map[bool]int{true: 0, false: 1}[tc.wantErr]; m.Available() != want {
				t.Errorf("want Available=%d, got %d", want, m.Available())
			}
			assertBinding(t, root, gpu.PCIAddress, "vfio-pci", "vfio-pci")
			assertBinding(t, root, audio, tc.wantDriver, "(null)")
		})
	}
}

// buildSharedGroupSysfs puts two GPUs and an audio function in IOMMU group 7.
func buildSharedGroupSysfs(t *testing.T, audioDriver string) (string, []GPUDevice) {
	t.Helper()
	root := t.TempDir()
	buildSysfsDevice(t, root, "0000:03:00.0", "0x030200", "0x10de", "0x2236", "nvidia", 7)
	buildSysfsDevice(t, root, "0000:03:00.1", "0x030200", "0x10de", "0x2236", "nvidia", 7)
	buildSysfsDevice(t, root, "0000:03:00.2", "0x040300", "0x10de", "0x1aef", audioDriver, 7)
	for _, d := range []string{"vfio-pci", "nvidia", "snd_hda_intel"} {
		makeSysfsDriverDir(t, root, d)
	}
	var gpus []GPUDevice
	for _, addr := range []string{"0000:03:00.0", "0000:03:00.1"} {
		gpus = append(gpus, GPUDevice{PCIAddress: addr, Vendor: VendorNVIDIA,
			VendorID: "10de", DeviceID: "2236", IOMMUGroup: 7, OriginalDriver: "nvidia"})
	}
	return root, gpus
}

func claimBoth(t *testing.T, m *Manager) {
	t.Helper()
	for range 2 {
		if _, _, err := m.Claim("i-001", ""); err != nil {
			t.Fatalf("Claim: %v", err)
		}
	}
}

func TestManagerRelease_TwoGPUsInOneGroupRestoreOnce(t *testing.T) {
	useKernfs(t)
	for _, rollback := range []bool{false, true} {
		t.Run(map[bool]string{false: "release", true: "rollback"}[rollback], func(t *testing.T) {
			root, gpus := buildSharedGroupSysfs(t, "snd_hda_intel")
			m := newManagerForTest(gpus, root)
			claimBoth(t, m)
			var err error
			if rollback {
				if _, _, cerr := m.Claim("i-001", ""); cerr == nil {
					t.Fatal("want a third Claim to fail on a two-GPU pool")
				}
				err = m.RollbackClaim("i-001")
			} else {
				err = m.Release("i-001")
			}
			if err != nil {
				t.Fatalf("release: %v", err)
			}
			assertBinding(t, root, "0000:03:00.0", "nvidia", "(null)")
			assertBinding(t, root, "0000:03:00.1", "nvidia", "(null)")
			assertBinding(t, root, "0000:03:00.2", "snd_hda_intel", "(null)")
			if m.Available() != 2 {
				t.Errorf("want Available=2, got %d", m.Available())
			}
		})
	}
}

func TestManagerRelease_SharedGroupFailureHoldsEveryEntry(t *testing.T) {
	useKernfs(t)
	root, gpus := buildSharedGroupSysfs(t, "snd_hda_intel")
	m := newManagerForTest(gpus, root)
	claimBoth(t, m)
	override := filepath.Join(root, "bus/pci/devices/0000:03:00.2/driver_override")
	must(t, os.Remove(override))
	must(t, os.Mkdir(override, 0o755))

	if err := m.Release("i-001"); err == nil {
		t.Fatal("want Release error when a shared group member cannot be restored")
	}
	if m.Available() != 0 {
		t.Errorf("want both GPUs held, got Available=%d", m.Available())
	}
}

// The audio function was unbound before the claims: a known empty original,
// so finding it unbound and cleared at release counts as restored.
func TestManagerRelease_SharedGroupKeepsKnownEmptyOriginal(t *testing.T) {
	useKernfs(t)
	const audio = "0000:03:00.2"
	root, gpus := buildSharedGroupSysfs(t, "")
	m := newManagerForTest(gpus, root)
	claimBoth(t, m)
	must(t, kernfsWrite(filepath.Join(root, "bus/pci/drivers/vfio-pci/unbind"), audio))
	must(t, kernfsWrite(filepath.Join(root, "bus/pci/devices", audio, "driver_override"), "\n"))

	if err := m.Release("i-001"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	assertBinding(t, root, audio, "", "(null)")
	if m.Available() != 2 {
		t.Errorf("want Available=2, got %d", m.Available())
	}
}

// GPUs with no IOMMU group are separate groups, so one failing does not hold the other.
func TestManagerRelease_UngroupedEntriesAreNotCoalesced(t *testing.T) {
	useKernfs(t)
	root := t.TempDir()
	makeSysfsDriverDir(t, root, "vfio-pci")
	makeSysfsDriverDir(t, root, "nvidia")
	var gpus []GPUDevice
	for _, addr := range []string{"0000:03:00.0", "0000:04:00.0"} {
		buildSysfsDevice(t, root, addr, "0x030200", "0x10de", "0x2236", "vfio-pci", -1)
		must(t, kernfsWrite(filepath.Join(root, "bus/pci/devices", addr, "driver_override"), "vfio-pci"))
		gpus = append(gpus, GPUDevice{PCIAddress: addr, Vendor: VendorNVIDIA,
			VendorID: "10de", DeviceID: "2236", IOMMUGroup: -1, OriginalDriver: "nvidia"})
	}
	m := newManagerForTest(gpus, root)
	for _, g := range gpus {
		if err := m.ReclaimByAddress(g.PCIAddress, "i-001"); err != nil {
			t.Fatalf("ReclaimByAddress %s: %v", g.PCIAddress, err)
		}
	}
	must(t, os.RemoveAll(filepath.Join(root, "bus/pci/devices/0000:04:00.0")))

	if err := m.Release("i-001"); err == nil {
		t.Fatal("want Release error for the missing GPU")
	}
	assertBinding(t, root, "0000:03:00.0", "nvidia", "(null)")
	if m.Available() != 1 {
		t.Errorf("want only the missing GPU held, got Available=%d", m.Available())
	}
}

// A claim moves the whole IOMMU group, so a failed restoration must also hold
// an idle GPU in that group: claiming it would take the half-restored members.
func TestManagerRelease_FailureFencesIdleSibling(t *testing.T) {
	useKernfs(t)
	for _, rollback := range []bool{false, true} {
		t.Run(map[bool]string{false: "release", true: "rollback"}[rollback], func(t *testing.T) {
			root, gpus := buildSharedGroupSysfs(t, "snd_hda_intel")
			m := newManagerForTest(gpus, root)
			if _, _, err := m.Claim("i-001", ""); err != nil {
				t.Fatalf("Claim: %v", err)
			}
			override := filepath.Join(root, "bus/pci/devices/0000:03:00.2/driver_override")
			must(t, os.Remove(override))
			must(t, os.Mkdir(override, 0o755))

			release := m.Release
			if rollback {
				release = m.RollbackClaim
			}
			if err := release("i-001"); err == nil {
				t.Fatal("want an error when a group member cannot be restored")
			}
			if m.Available() != 0 {
				t.Errorf("want the idle sibling held too, got Available=%d", m.Available())
			}
			if _, _, err := m.Claim("i-002", ""); err == nil {
				t.Error("want Claim to refuse the idle sibling of a failed group")
			}
		})
	}
}

func TestManagerClaim_FailedRollbackFencesIdleSibling(t *testing.T) {
	useKernfs(t)
	root, gpus := buildSharedGroupSysfs(t, "snd_hda_intel")
	// The audio function cannot bind to vfio-pci, and the GPUs cannot return to nvidia.
	must(t, os.RemoveAll(filepath.Join(root, "bus/pci/devices/0000:03:00.2/driver_override")))
	must(t, os.Mkdir(filepath.Join(root, "bus/pci/devices/0000:03:00.2/driver_override"), 0o755))
	must(t, os.Mkdir(filepath.Join(root, "bus/pci/drivers/nvidia/bind"), 0o755))
	m := newManagerForTest(gpus, root)

	if _, _, err := m.Claim("i-001", ""); err == nil {
		t.Fatal("want Claim error when the audio function fails to bind")
	}
	if m.Available() != 0 {
		t.Errorf("want both GPUs in the group held, got Available=%d", m.Available())
	}
}

// captureLogs routes slog to a JSON buffer for the rest of the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// Both GPUs of the group fail to return to nvidia during one claim rollback: each
// failure is logged, and the group is fenced once, so each GPU is reported once.
func TestManagerClaim_RollbackFailuresFenceGroupOnce(t *testing.T) {
	useKernfs(t)
	root, gpus := buildSharedGroupSysfs(t, "snd_hda_intel")
	must(t, os.RemoveAll(filepath.Join(root, "bus/pci/devices/0000:03:00.2/driver_override")))
	must(t, os.Mkdir(filepath.Join(root, "bus/pci/devices/0000:03:00.2/driver_override"), 0o755))
	must(t, os.Mkdir(filepath.Join(root, "bus/pci/drivers/nvidia/bind"), 0o755))
	m := newManagerForTest(gpus, root)
	logs := captureLogs(t)

	if _, _, err := m.Claim("i-001", ""); err == nil {
		t.Fatal("want Claim error when the audio function fails to bind")
	}
	if m.Available() != 0 {
		t.Errorf("want both GPUs in the group held, got Available=%d", m.Available())
	}
	if _, _, err := m.Claim("i-002", ""); err == nil {
		t.Error("want Claim to refuse a GPU of the fenced group")
	}

	rollbackFailed := map[string]int{}
	fenced := map[string]int{}
	for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
		var rec struct{ Msg, GPU, Addr string }
		must(t, json.Unmarshal(line, &rec))
		switch {
		case strings.HasPrefix(rec.Msg, "GPU claim rollback failed"):
			rollbackFailed[rec.Addr]++
		case strings.HasPrefix(rec.Msg, "GPU marked unavailable"):
			fenced[rec.GPU]++
		}
	}
	for _, addr := range []string{"0000:03:00.0", "0000:03:00.1"} {
		if rollbackFailed[addr] != 1 {
			t.Errorf("rollback failure for %s logged %d times, want 1", addr, rollbackFailed[addr])
		}
		if fenced[addr] != 1 {
			t.Errorf("fence of %s logged %d times, want 1", addr, fenced[addr])
		}
	}
	if len(rollbackFailed) != 2 || len(fenced) != 2 {
		t.Errorf("unexpected log targets: rollback %v, fenced %v", rollbackFailed, fenced)
	}
}

// restoreExternally moves addr back to driver with a literal "(null)" override,
// which reads exactly like a cleared one.
func restoreExternally(t *testing.T, root, addr, driver string) {
	t.Helper()
	devPath := filepath.Join(root, "bus/pci/devices", addr)
	must(t, kernfsWrite(filepath.Join(root, "bus/pci/drivers/vfio-pci/unbind"), addr))
	must(t, kernfsWrite(filepath.Join(devPath, "driver_override"), "\n"))
	must(t, kernfsWrite(filepath.Join(root, "bus/pci/drivers", driver, "bind"), addr))
	must(t, kernfsWrite(filepath.Join(devPath, "driver_override"), "(null)"))
}

func TestManagerRelease_ClearsLiteralNullOnRestoredMember(t *testing.T) {
	useKernfs(t)
	const audio = "0000:03:00.2"
	root, gpus := buildSharedGroupSysfs(t, "snd_hda_intel")
	m := newManagerForTest(gpus[:1], root)
	if _, _, err := m.Claim("i-001", ""); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	restoreExternally(t, root, audio, "snd_hda_intel")

	if err := m.Release("i-001"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := kernfsOverride(filepath.Join(root, "bus/pci/devices", audio)); got != "" {
		t.Errorf("audio override after release = %q, want it cleared", got)
	}
}

func TestManagerRelease_FailedClearOnRestoredMemberFencesGroup(t *testing.T) {
	useKernfs(t)
	const audio = "0000:03:00.2"
	root, gpus := buildSharedGroupSysfs(t, "snd_hda_intel")
	m := newManagerForTest(gpus, root)
	if _, _, err := m.Claim("i-001", ""); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	restoreExternally(t, root, audio, "snd_hda_intel")
	override := filepath.Join(root, "bus/pci/devices", audio, "driver_override")
	sysfsWrite = func(path, value string) error {
		if path == override {
			return syscall.EIO
		}
		return kernfsWrite(path, value)
	}

	if err := m.Release("i-001"); err == nil {
		t.Fatal("want Release error when the override clear fails")
	}
	if m.Available() != 0 {
		t.Errorf("want both GPUs in the group held, got Available=%d", m.Available())
	}
}

func TestManagerRollback_LeavesPreBoundMemberUnwritten(t *testing.T) {
	useKernfs(t)
	root := t.TempDir()
	buildSysfsDevice(t, root, "0000:03:00.0", "0x030200", "0x10de", "0x2236", "vfio-pci", 7)
	buildSysfsDevice(t, root, "0000:03:00.1", "0x040300", "0x10de", "0x1aef", "snd_hda_intel", 7)
	for _, d := range []string{"vfio-pci", "nvidia", "snd_hda_intel"} {
		makeSysfsDriverDir(t, root, d)
	}
	gpuDev := filepath.Join(root, "bus/pci/devices/0000:03:00.0")
	must(t, kernfsWrite(filepath.Join(gpuDev, "driver_override"), "vfio-pci"))
	m := newManagerForTest([]GPUDevice{{PCIAddress: "0000:03:00.0", Vendor: VendorNVIDIA,
		VendorID: "10de", DeviceID: "2236", IOMMUGroup: 7, OriginalDriver: "nvidia"}}, root)
	if _, _, err := m.Claim("i-001", ""); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	var written []string
	sysfsWrite = func(path, value string) error {
		written = append(written, path)
		return kernfsWrite(path, value)
	}

	if err := m.RollbackClaim("i-001"); err != nil {
		t.Fatalf("RollbackClaim: %v", err)
	}
	for _, path := range written {
		if strings.HasPrefix(path, gpuDev+"/") {
			t.Errorf("rollback wrote %s on the pre-bound GPU", path)
		}
	}
	assertBinding(t, root, "0000:03:00.0", "vfio-pci", "vfio-pci")
	assertBinding(t, root, "0000:03:00.1", "snd_hda_intel", "(null)")
}

// The claim that moved a function holds its true original, even an empty one;
// a later claim of the same group sees it on vfio-pci and only has discovery.
func TestManagerRelease_MoverRecordBeatsDiscoveryDriver(t *testing.T) {
	useKernfs(t)
	const second = "0000:03:00.1"
	for _, mover := range []bool{true, false} {
		t.Run(map[bool]string{true: "mover", false: "no mover"}[mover], func(t *testing.T) {
			root, gpus := buildSharedGroupSysfs(t, "snd_hda_intel")
			if mover {
				must(t, kernfsWrite(filepath.Join(root, "bus/pci/drivers/nvidia/unbind"), second))
			} else {
				must(t, kernfsWrite(filepath.Join(root, "bus/pci/drivers/nvidia/unbind"), second))
				must(t, kernfsWrite(filepath.Join(root, "bus/pci/devices", second, "driver_override"), "vfio-pci"))
				must(t, kernfsWrite(filepath.Join(root, "bus/pci/drivers/vfio-pci/bind"), second))
			}
			// Pool order puts the second GPU first, so its discovery record is merged first.
			m := newManagerForTest([]GPUDevice{gpus[1], gpus[0]}, root)
			m.pool[0].Available = false
			if _, _, err := m.Claim("i-001", ""); err != nil {
				t.Fatalf("first Claim: %v", err)
			}
			m.pool[0].Available = true
			if _, _, err := m.Claim("i-001", ""); err != nil {
				t.Fatalf("second Claim: %v", err)
			}

			if err := m.Release("i-001"); err != nil {
				t.Fatalf("Release: %v", err)
			}
			if mover {
				assertBinding(t, root, second, "", "(null)")
			} else {
				assertBinding(t, root, second, "nvidia", "(null)")
			}
			if m.Available() != 2 {
				t.Errorf("want Available=2, got %d", m.Available())
			}
		})
	}
}
