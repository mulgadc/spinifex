//test:in-package — swaps the unexported sysfs writer for tests that need kernfs semantics.
package gpu

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// useKernfs routes this test's sysfs writes through a model of PCI sysfs, as a
// regular file truncates on an empty write and never moves a driver link.
// The package's tests do not run in parallel, so swapping the writer is safe.
func useKernfs(t *testing.T) {
	t.Helper()
	sysfsWrite = kernfsWrite
	kernfsSet = map[string]string{}
	t.Cleanup(func() { sysfsWrite = writeSysfs; kernfsSet = map[string]string{} })
}

// kernfsSet holds each device's true override, keyed by device path, since the
// kernel shows an unset override and a literal "(null)" alike.
var kernfsSet = map[string]string{}

// kernfsWrite models PCI sysfs: opening without O_TRUNC keeps path errors but
// not truncation, a zero-length write never reaches the store, a newline clears
// the override, and bind refuses a device overridden to another driver.
func kernfsWrite(path, value string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	dir, attr := filepath.Dir(path), filepath.Base(path)
	switch {
	case value == "":
		return nil
	case attr == "driver_override":
		v, _, _ := strings.Cut(value, "\n")
		kernfsSet[dir] = v
		if v == "" {
			v = "(null)"
		}
		return os.WriteFile(path, []byte(v+"\n"), 0o600)
	case (attr == "bind" || attr == "unbind") && filepath.Base(filepath.Dir(dir)) == "drivers":
		devPath := filepath.Join(filepath.Dir(filepath.Dir(dir)), "devices", value)
		link := filepath.Join(devPath, "driver")
		if attr == "bind" {
			if o := kernfsOverride(devPath); o != "" && o != filepath.Base(dir) {
				return syscall.ENODEV
			}
		}
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			return err
		}
		if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if attr == "unbind" {
			return nil
		}
		return os.Symlink(dir, link)
	default:
		return os.WriteFile(path, []byte(value), 0o600)
	}
}

// kernfsOverride returns the driver a device's override names, or "" if unset.
func kernfsOverride(devPath string) string {
	if v, ok := kernfsSet[devPath]; ok {
		return v
	}
	b, err := os.ReadFile(filepath.Join(devPath, "driver_override"))
	if err != nil {
		return ""
	}
	if v := strings.TrimSuffix(string(b), "\n"); v != "(null)" {
		return v
	}
	return ""
}

func TestKernfsWrite_ModelsOverrideAndBind(t *testing.T) {
	root := t.TempDir()
	buildSysfsDevice(t, root, "0000:03:00.0", "0x030200", "0x10de", "0x2236", "", 7)
	makeSysfsDriverDir(t, root, "vfio-pci")
	makeSysfsDriverDir(t, root, "nvidia")
	devPath := filepath.Join(root, "bus/pci/devices/0000:03:00.0")
	override := filepath.Join(devPath, "driver_override")
	nvidiaBind := filepath.Join(root, "bus/pci/drivers/nvidia/bind")

	must(t, kernfsWrite(override, "vfio-pci"))
	must(t, kernfsWrite(override, ""))
	if got := readSysfsTestFile(t, override); got != "vfio-pci\n" {
		t.Fatalf("override after empty write = %q, want it unchanged", got)
	}
	if err := kernfsWrite(nvidiaBind, "0000:03:00.0"); !errors.Is(err, syscall.ENODEV) {
		t.Fatalf("bind against another driver's override = %v, want ENODEV", err)
	}
	must(t, kernfsWrite(override, "\n"))
	if got := readSysfsTestFile(t, override); got != "(null)\n" {
		t.Fatalf("override after newline = %q, want (null)", got)
	}
	must(t, kernfsWrite(nvidiaBind, "0000:03:00.0"))
	if got, _ := readDriver(devPath); got != "nvidia" {
		t.Fatalf("driver after bind = %q, want nvidia", got)
	}
	must(t, kernfsWrite(filepath.Join(root, "bus/pci/drivers/nvidia/unbind"), "0000:03:00.0"))
	if got, _ := readDriver(devPath); got != "" {
		t.Fatalf("driver after unbind = %q, want unbound", got)
	}
}

func TestKernfsWrite_LiteralNullOverrideBlocksBind(t *testing.T) {
	useKernfs(t)
	root := t.TempDir()
	buildSysfsDevice(t, root, "0000:03:00.0", "0x030200", "0x10de", "0x2236", "", 7)
	makeSysfsDriverDir(t, root, "nvidia")
	override := filepath.Join(root, "bus/pci/devices/0000:03:00.0/driver_override")
	nvidiaBind := filepath.Join(root, "bus/pci/drivers/nvidia/bind")

	must(t, kernfsWrite(override, "(null)"))
	if got := readSysfsTestFile(t, override); got != "(null)\n" {
		t.Fatalf("override after a literal (null) = %q, want it shown as (null)", got)
	}
	if err := kernfsWrite(nvidiaBind, "0000:03:00.0"); !errors.Is(err, syscall.ENODEV) {
		t.Fatalf("bind with a literal (null) override = %v, want ENODEV", err)
	}
	must(t, kernfsWrite(override, "\n"))
	must(t, kernfsWrite(nvidiaBind, "0000:03:00.0"))
}

func TestKernfsWrite_WhitespaceOverrideBlocksBind(t *testing.T) {
	root := t.TempDir()
	buildSysfsDevice(t, root, "0000:03:00.0", "0x030200", "0x10de", "0x2236", "", 7)
	makeSysfsDriverDir(t, root, "nvidia")
	override := filepath.Join(root, "bus/pci/devices/0000:03:00.0/driver_override")

	must(t, kernfsWrite(override, " "))
	if got := readSysfsTestFile(t, override); got != " \n" {
		t.Fatalf("override after a space = %q, want it kept", got)
	}
	if err := kernfsWrite(filepath.Join(root, "bus/pci/drivers/nvidia/bind"), "0000:03:00.0"); !errors.Is(err, syscall.ENODEV) {
		t.Fatalf("bind with a whitespace override = %v, want ENODEV", err)
	}
}

// The production writer must hand the kernel the exact bytes, newline included.
func TestUnbindVFIO_ProductionWriterSendsNewlineClear(t *testing.T) {
	root := t.TempDir()
	buildSysfsDevice(t, root, "0000:03:00.0", "0x030200", "0x10de", "0x2236", "vfio-pci", 7)
	makeSysfsDriverDir(t, root, "vfio-pci")
	makeSysfsDriverDir(t, root, "nvidia")
	devPath := filepath.Join(root, "bus/pci/devices/0000:03:00.0")
	must(t, os.WriteFile(filepath.Join(devPath, "driver_override"), []byte("vfio-pci\n"), 0o600))

	must(t, unbindVFIO(root, "0000:03:00.0", "nvidia"))
	for path, want := range map[string]string{
		filepath.Join(root, "bus/pci/drivers/vfio-pci/unbind"): "0000:03:00.0",
		filepath.Join(devPath, "driver_override"):              "\n",
		filepath.Join(root, "bus/pci/drivers/nvidia/bind"):     "0000:03:00.0",
	} {
		if got := readSysfsTestFile(t, path); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
}

func TestWriteSysfs_ReportsMissingAttributeDir(t *testing.T) {
	if err := writeSysfs(filepath.Join(t.TempDir(), "missing/bind"), "0000:03:00.0"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("writeSysfs into a missing dir = %v, want ErrNotExist", err)
	}
}
