package gpu

import (
	"os"
	"path/filepath"
	"testing"
)

// makeResetNode pre-creates the sysfs reset attribute for a device.
func makeResetNode(t *testing.T, root, pciAddr string) {
	t.Helper()
	devPath := filepath.Join(root, "bus/pci/devices", pciAddr)
	must(t, os.WriteFile(filepath.Join(devPath, "reset"), []byte(""), 0o600))
}

func TestResetPCI_AMDSupported(t *testing.T) {
	root := t.TempDir()
	// AMD vendor 0x1002
	buildSysfsDevice(t, root, "0000:05:00.0", "0x030200", "0x1002", "0x744c", "vfio-pci", 3)
	makeResetNode(t, root, "0000:05:00.0")

	if err := resetPCI(root, "0000:05:00.0"); err != nil {
		t.Fatalf("resetPCI: %v", err)
	}

	got := readSysfsTestFile(t, filepath.Join(root, "bus/pci/devices/0000:05:00.0/reset"))
	if got != "1" {
		t.Errorf("reset node = %q, want \"1\"", got)
	}
}

func TestResetPCI_AMDNoFLR(t *testing.T) {
	root := t.TempDir()
	buildSysfsDevice(t, root, "0000:05:00.0", "0x030200", "0x1002", "0x744c", "vfio-pci", 3)
	// No reset attribute — AMD device does not expose FLR.

	if err := resetPCI(root, "0000:05:00.0"); err != nil {
		t.Fatalf("resetPCI should return nil when FLR node is absent, got: %v", err)
	}
}

func TestResetPCI_NVIDIASkipped(t *testing.T) {
	root := t.TempDir()
	// NVIDIA vendor 0x10de — reset should be skipped entirely.
	buildSysfsDevice(t, root, "0000:03:00.0", "0x030200", "0x10de", "0x2236", "vfio-pci", 7)
	makeResetNode(t, root, "0000:03:00.0")

	if err := resetPCI(root, "0000:03:00.0"); err != nil {
		t.Fatalf("resetPCI: %v", err)
	}

	// Reset node must NOT have been written for a non-AMD device.
	got := readSysfsTestFile(t, filepath.Join(root, "bus/pci/devices/0000:03:00.0/reset"))
	if got == "1" {
		t.Error("reset node was written for NVIDIA device — should be skipped")
	}
}

func TestResetPCI_WriteFails(t *testing.T) {
	root := t.TempDir()
	buildSysfsDevice(t, root, "0000:05:00.0", "0x030200", "0x1002", "0x744c", "vfio-pci", 3)
	devPath := filepath.Join(root, "bus/pci/devices/0000:05:00.0")
	must(t, os.WriteFile(filepath.Join(devPath, "reset"), []byte(""), 0o444)) // read-only

	if err := resetPCI(root, "0000:05:00.0"); err == nil {
		t.Fatal("resetPCI should return error when reset write fails, got nil")
	}
}

// TestUnbindVFIO_AMDResetIssued verifies the reset node is written when an AMD
// device is unbound from vfio-pci.
func TestUnbindVFIO_AMDResetIssued(t *testing.T) {
	root := t.TempDir()
	buildSysfsDevice(t, root, "0000:05:00.0", "0x030200", "0x1002", "0x744c", "vfio-pci", 3)
	makeSysfsDriverDir(t, root, "vfio-pci")
	makeSysfsDriverDir(t, root, "amdgpu")
	makeResetNode(t, root, "0000:05:00.0")

	if err := unbindVFIO(root, "0000:05:00.0", "amdgpu"); err != nil {
		t.Fatalf("unbindVFIO: %v", err)
	}

	if got := readSysfsTestFile(t, filepath.Join(root, "bus/pci/devices/0000:05:00.0/reset")); got != "1" {
		t.Errorf("reset node = %q, want \"1\" after unbindVFIO for AMD device", got)
	}
	if got := readSysfsTestFile(t, filepath.Join(root, "bus/pci/drivers/amdgpu/bind")); got != "0000:05:00.0" {
		t.Errorf("amdgpu/bind = %q, want 0000:05:00.0", got)
	}
}

// TestUnbindVFIO_NVIDIAResetSkipped verifies no reset is issued for NVIDIA.
func TestUnbindVFIO_NVIDIAResetSkipped(t *testing.T) {
	root := t.TempDir()
	buildSysfsDevice(t, root, "0000:03:00.0", "0x030200", "0x10de", "0x2236", "vfio-pci", 7)
	makeSysfsDriverDir(t, root, "vfio-pci")
	makeSysfsDriverDir(t, root, "nvidia")
	makeResetNode(t, root, "0000:03:00.0")

	if err := unbindVFIO(root, "0000:03:00.0", "nvidia"); err != nil {
		t.Fatalf("unbindVFIO: %v", err)
	}

	// Reset node must not have been written for NVIDIA.
	got := readSysfsTestFile(t, filepath.Join(root, "bus/pci/devices/0000:03:00.0/reset"))
	if got == "1" {
		t.Error("reset node was written for NVIDIA device during unbindVFIO — should be skipped")
	}
	if got := readSysfsTestFile(t, filepath.Join(root, "bus/pci/drivers/nvidia/bind")); got != "0000:03:00.0" {
		t.Errorf("nvidia/bind = %q, want 0000:03:00.0", got)
	}
}
