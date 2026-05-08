package gpu

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// buildSysfsDevice creates a fake sysfs PCI device under root.
// driver and iommuGroup are optional: pass "" / -1 to omit.
func buildSysfsDevice(t *testing.T, root, pciAddr, class, vendor, device, driver string, iommuGroup int) {
	t.Helper()
	devPath := filepath.Join(root, "bus/pci/devices", pciAddr)
	must(t, os.MkdirAll(devPath, 0755))
	writeFile(t, filepath.Join(devPath, "class"), class)
	writeFile(t, filepath.Join(devPath, "vendor"), vendor)
	writeFile(t, filepath.Join(devPath, "device"), device)

	if iommuGroup >= 0 {
		groupPath := filepath.Join(root, "kernel/iommu_groups", strconv.Itoa(iommuGroup))
		must(t, os.MkdirAll(filepath.Join(groupPath, "devices"), 0755))
		// symlink: <devPath>/iommu_group -> <groupPath>
		must(t, os.Symlink(groupPath, filepath.Join(devPath, "iommu_group")))
		// symlink: <groupPath>/devices/<pciAddr> -> <devPath>
		must(t, os.Symlink(devPath, filepath.Join(groupPath, "devices", pciAddr)))
	}

	if driver != "" {
		driverPath := filepath.Join(root, "bus/pci/drivers", driver)
		must(t, os.MkdirAll(driverPath, 0755))
		must(t, os.Symlink(driverPath, filepath.Join(devPath, "driver")))
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	must(t, os.WriteFile(path, []byte(content+"\n"), 0444))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestDiscover_NVIDIA3DController(t *testing.T) {
	root := t.TempDir()
	buildSysfsDevice(t, root, "0000:03:00.0", "0x030200", "0x10de", "0x2236", "nvidia", 7)

	gpus, err := discover(root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(gpus) != 1 {
		t.Fatalf("want 1 GPU, got %d", len(gpus))
	}

	g := gpus[0]
	if g.PCIAddress != "0000:03:00.0" {
		t.Errorf("PCIAddress = %q, want 0000:03:00.0", g.PCIAddress)
	}
	if g.Vendor != VendorNVIDIA {
		t.Errorf("Vendor = %q, want nvidia", g.Vendor)
	}
	if g.VendorID != "10de" {
		t.Errorf("VendorID = %q, want 10de", g.VendorID)
	}
	if g.DeviceID != "2236" {
		t.Errorf("DeviceID = %q, want 2236", g.DeviceID)
	}
	if g.IOMMUGroup != 7 {
		t.Errorf("IOMMUGroup = %d, want 7", g.IOMMUGroup)
	}
	if g.OriginalDriver != "nvidia" {
		t.Errorf("OriginalDriver = %q, want nvidia", g.OriginalDriver)
	}
	if g.Model != "NVIDIA A10" {
		t.Errorf("Model = %q, want NVIDIA A10", g.Model)
	}
	if g.MemoryMiB != 23028 {
		t.Errorf("MemoryMiB = %d, want 23028", g.MemoryMiB)
	}
}

func TestDiscover_AMDVGAController(t *testing.T) {
	root := t.TempDir()
	buildSysfsDevice(t, root, "0000:05:00.0", "0x030000", "0x1002", "0x744c", "amdgpu", 3)

	gpus, err := discover(root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(gpus) != 1 {
		t.Fatalf("want 1 GPU, got %d", len(gpus))
	}

	g := gpus[0]
	if g.Vendor != VendorAMD {
		t.Errorf("Vendor = %q, want amd", g.Vendor)
	}
	if g.OriginalDriver != "amdgpu" {
		t.Errorf("OriginalDriver = %q, want amdgpu", g.OriginalDriver)
	}
}

func TestDiscover_SkipsNonDisplay(t *testing.T) {
	root := t.TempDir()
	// NVMe — class 0x010802 (storage controller)
	buildSysfsDevice(t, root, "0000:01:00.0", "0x010802", "0x144d", "0xa809", "nvme", 1)
	// USB controller — class 0x0c0330
	buildSysfsDevice(t, root, "0000:00:14.0", "0x0c0330", "0x8086", "0x9d2f", "xhci_hcd", 2)

	gpus, err := discover(root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(gpus) != 0 {
		t.Errorf("want 0 GPUs, got %d", len(gpus))
	}
}

func TestDiscover_NoIOMMU(t *testing.T) {
	root := t.TempDir()
	// GPU with no iommu_group symlink (IOMMU disabled)
	buildSysfsDevice(t, root, "0000:03:00.0", "0x030200", "0x10de", "0x2236", "nvidia", -1)

	gpus, err := discover(root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(gpus) != 1 {
		t.Fatalf("want 1 GPU, got %d", len(gpus))
	}
	if gpus[0].IOMMUGroup != -1 {
		t.Errorf("IOMMUGroup = %d, want -1 when IOMMU inactive", gpus[0].IOMMUGroup)
	}
}

func TestDiscover_UnboundDriver(t *testing.T) {
	root := t.TempDir()
	// No driver symlink
	buildSysfsDevice(t, root, "0000:03:00.0", "0x030200", "0x10de", "0x2236", "", 7)

	gpus, err := discover(root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if gpus[0].OriginalDriver != "" {
		t.Errorf("OriginalDriver = %q, want empty for unbound device", gpus[0].OriginalDriver)
	}
}

func TestDiscover_UnknownDevice(t *testing.T) {
	root := t.TempDir()
	// NVIDIA device not in the model table
	buildSysfsDevice(t, root, "0000:03:00.0", "0x030200", "0x10de", "0xffff", "nvidia", 7)

	gpus, err := discover(root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(gpus) != 1 {
		t.Fatalf("want 1 GPU, got %d", len(gpus))
	}
	if gpus[0].Model == "" {
		t.Error("Model should be non-empty fallback for unknown device IDs")
	}
	if gpus[0].MemoryMiB != 0 {
		t.Errorf("MemoryMiB = %d, want 0 for unknown device", gpus[0].MemoryMiB)
	}
}

func TestDiscover_MultipleDevices(t *testing.T) {
	root := t.TempDir()
	buildSysfsDevice(t, root, "0000:03:00.0", "0x030200", "0x10de", "0x2236", "nvidia", 7)
	buildSysfsDevice(t, root, "0000:04:00.0", "0x030200", "0x10de", "0x2236", "nvidia", 8)
	buildSysfsDevice(t, root, "0000:01:00.0", "0x010802", "0x144d", "0xa809", "nvme", 1) // non-GPU

	gpus, err := discover(root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(gpus) != 2 {
		t.Errorf("want 2 GPUs, got %d", len(gpus))
	}
}

func TestDiscover_ReadDirError(t *testing.T) {
	// Pass a sysfsRoot with no bus/pci/devices directory.
	_, err := discover(t.TempDir())
	if err == nil {
		t.Error("want error when devices directory is missing, got nil")
	}
}

func TestDiscover_SkipsIntelGPU(t *testing.T) {
	root := t.TempDir()
	// Intel iGPU (vendor 0x8086) must be skipped even though it is a display class.
	buildSysfsDevice(t, root, "0000:00:02.0", "0x030000", "0x8086", "0x46a8", "i915", 1)

	gpus, err := discover(root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(gpus) != 0 {
		t.Errorf("want Intel GPU skipped, got %d GPUs", len(gpus))
	}
}

func TestIsDisplayClass(t *testing.T) {
	cases := []struct {
		class string
		want  bool
	}{
		{"0x030200", true},  // 3D controller
		{"0x030000", true},  // VGA
		{"0x038000", true},  // display controller
		{"0x010802", false}, // NVMe
		{"0x0c0330", false}, // USB
		{"0x020000", false}, // Ethernet
	}
	for _, c := range cases {
		if got := isDisplayClass(c.class); got != c.want {
			t.Errorf("isDisplayClass(%q) = %v, want %v", c.class, got, c.want)
		}
	}
}
