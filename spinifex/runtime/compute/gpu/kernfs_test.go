//test:in-package — replaces the unexported sysfs writer for the whole package.
package gpu

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Every test in the package writes through a model of PCI sysfs, because a
// regular file truncates on an empty write and never moves a driver link.
func init() { sysfsWrite = kernfsWrite }

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
	b, err := os.ReadFile(filepath.Join(devPath, "driver_override"))
	if err != nil {
		return ""
	}
	if v := strings.TrimSpace(string(b)); v != "(null)" {
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
