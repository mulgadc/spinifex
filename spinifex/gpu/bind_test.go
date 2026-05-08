package gpu

import (
	"os"
	"path/filepath"
	"testing"
)

// makeSysfsDriverDir creates a driver directory under root/bus/pci/drivers/<name>.
func makeSysfsDriverDir(t *testing.T, root, driver string) string {
	t.Helper()
	path := filepath.Join(root, "bus/pci/drivers", driver)
	must(t, os.MkdirAll(path, 0o755))
	return path
}

// readSysfsTestFile reads a file written by writeSysfs in a test sysfs tree.
func readSysfsTestFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestBindVFIO_BoundToDriver(t *testing.T) {
	root := t.TempDir()
	buildSysfsDevice(t, root, "0000:03:00.0", "0x030200", "0x10de", "0x2236", "nvidia", 7)
	makeSysfsDriverDir(t, root, "vfio-pci")

	orig, err := bindVFIO(root, "0000:03:00.0")
	if err != nil {
		t.Fatalf("bindVFIO: %v", err)
	}
	if orig != "nvidia" {
		t.Errorf("original driver = %q, want nvidia", orig)
	}

	devPath := filepath.Join(root, "bus/pci/devices/0000:03:00.0")

	if got := readSysfsTestFile(t, filepath.Join(root, "bus/pci/drivers/nvidia/unbind")); got != "0000:03:00.0" {
		t.Errorf("nvidia/unbind = %q, want 0000:03:00.0", got)
	}
	if got := readSysfsTestFile(t, filepath.Join(devPath, "driver_override")); got != "vfio-pci" {
		t.Errorf("driver_override = %q, want vfio-pci", got)
	}
	if got := readSysfsTestFile(t, filepath.Join(root, "bus/pci/drivers/vfio-pci/bind")); got != "0000:03:00.0" {
		t.Errorf("vfio-pci/bind = %q, want 0000:03:00.0", got)
	}
}

func TestBindVFIO_AlreadyBoundToVFIO(t *testing.T) {
	root := t.TempDir()
	buildSysfsDevice(t, root, "0000:03:00.0", "0x030200", "0x10de", "0x2236", "vfio-pci", 7)

	orig, err := bindVFIO(root, "0000:03:00.0")
	if err != nil {
		t.Fatalf("bindVFIO: %v", err)
	}
	if orig != "vfio-pci" {
		t.Errorf("expected vfio-pci for idempotent call, got %q", orig)
	}

	// No vfio-pci/bind file should have been written (idempotent).
	bindPath := filepath.Join(root, "bus/pci/drivers/vfio-pci/bind")
	if _, err := os.Stat(bindPath); !os.IsNotExist(err) {
		t.Error("vfio-pci/bind should not be written for already-bound device")
	}
}

func TestBindVFIO_Unbound(t *testing.T) {
	root := t.TempDir()
	// No driver symlink — device is unbound.
	buildSysfsDevice(t, root, "0000:03:00.0", "0x030200", "0x10de", "0x2236", "", 7)
	makeSysfsDriverDir(t, root, "vfio-pci")

	orig, err := bindVFIO(root, "0000:03:00.0")
	if err != nil {
		t.Fatalf("bindVFIO: %v", err)
	}
	if orig != "" {
		t.Errorf("original driver = %q, want empty for unbound device", orig)
	}

	// No unbind file should have been written (nothing to unbind).
	nvidiaUnbind := filepath.Join(root, "bus/pci/drivers/nvidia/unbind")
	if _, err := os.Stat(nvidiaUnbind); !os.IsNotExist(err) {
		t.Error("no driver unbind should be written when device was unbound")
	}

	devPath := filepath.Join(root, "bus/pci/devices/0000:03:00.0")
	if got := readSysfsTestFile(t, filepath.Join(devPath, "driver_override")); got != "vfio-pci" {
		t.Errorf("driver_override = %q, want vfio-pci", got)
	}
}

func TestUnbindVFIO_WithOriginalDriver(t *testing.T) {
	root := t.TempDir()
	buildSysfsDevice(t, root, "0000:03:00.0", "0x030200", "0x10de", "0x2236", "vfio-pci", 7)
	makeSysfsDriverDir(t, root, "vfio-pci")
	makeSysfsDriverDir(t, root, "nvidia")

	err := unbindVFIO(root, "0000:03:00.0", "nvidia")
	if err != nil {
		t.Fatalf("unbindVFIO: %v", err)
	}

	devPath := filepath.Join(root, "bus/pci/devices/0000:03:00.0")

	if got := readSysfsTestFile(t, filepath.Join(root, "bus/pci/drivers/vfio-pci/unbind")); got != "0000:03:00.0" {
		t.Errorf("vfio-pci/unbind = %q, want 0000:03:00.0", got)
	}
	if got := readSysfsTestFile(t, filepath.Join(devPath, "driver_override")); got != "" {
		t.Errorf("driver_override = %q, want empty after unbind", got)
	}
	if got := readSysfsTestFile(t, filepath.Join(root, "bus/pci/drivers/nvidia/bind")); got != "0000:03:00.0" {
		t.Errorf("nvidia/bind = %q, want 0000:03:00.0", got)
	}
}

func TestBindVFIO_VfioBindFails(t *testing.T) {
	root := t.TempDir()
	buildSysfsDevice(t, root, "0000:03:00.0", "0x030200", "0x10de", "0x2236", "nvidia", 7)
	// nvidia dir is created by buildSysfsDevice; deliberately omit vfio-pci dir so bind fails.

	_, err := bindVFIO(root, "0000:03:00.0")
	if err == nil {
		t.Error("want error when vfio-pci bind directory is missing, got nil")
	}
}

func TestUnbindVFIO_ClearDriverOverrideFails(t *testing.T) {
	root := t.TempDir()
	buildSysfsDevice(t, root, "0000:03:00.0", "0x030200", "0x10de", "0x2236", "vfio-pci", 7)
	makeSysfsDriverDir(t, root, "vfio-pci")
	makeSysfsDriverDir(t, root, "nvidia")

	// Pre-create driver_override as read-only so the clear write fails.
	devPath := filepath.Join(root, "bus/pci/devices/0000:03:00.0")
	must(t, os.WriteFile(filepath.Join(devPath, "driver_override"), []byte("vfio-pci"), 0o444))

	err := unbindVFIO(root, "0000:03:00.0", "nvidia")
	if err == nil {
		t.Error("want error when driver_override clear fails, got nil")
	}
}

func TestUnbindVFIO_NoOriginalDriver(t *testing.T) {
	root := t.TempDir()
	buildSysfsDevice(t, root, "0000:03:00.0", "0x030200", "0x10de", "0x2236", "vfio-pci", 7)
	makeSysfsDriverDir(t, root, "vfio-pci")

	err := unbindVFIO(root, "0000:03:00.0", "")
	if err != nil {
		t.Fatalf("unbindVFIO: %v", err)
	}

	// With no original driver there should be no rebind file written.
	matches, _ := filepath.Glob(filepath.Join(root, "bus/pci/drivers/*/bind"))
	if len(matches) > 0 {
		t.Errorf("no driver rebind expected when originalDriver is empty, got: %v", matches)
	}
}
