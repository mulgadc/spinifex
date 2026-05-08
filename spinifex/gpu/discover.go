package gpu

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Discover enumerates GPUs on the host via sysfs and returns a GPUDevice for
// each display-class PCI device found. nvidia-smi is used to enrich NVIDIA
// entries if available; falls back to the built-in model table otherwise.
func Discover() ([]GPUDevice, error) {
	return discover("/sys")
}

func discover(sysfsRoot string) ([]GPUDevice, error) {
	devicesPath := filepath.Join(sysfsRoot, "bus/pci/devices")
	entries, err := os.ReadDir(devicesPath)
	if err != nil {
		return nil, fmt.Errorf("read pci devices: %w", err)
	}

	var gpus []GPUDevice
	for _, entry := range entries {
		devPath := filepath.Join(devicesPath, entry.Name())

		class, err := readSysfsString(filepath.Join(devPath, "class"))
		if err != nil || !isDisplayClass(class) {
			continue
		}

		vendorRaw, err := readSysfsString(filepath.Join(devPath, "vendor"))
		if err != nil {
			slog.Debug("gpu discover: skip device, cannot read vendor", "addr", entry.Name(), "err", err)
			continue
		}
		deviceRaw, err := readSysfsString(filepath.Join(devPath, "device"))
		if err != nil {
			slog.Debug("gpu discover: skip device, cannot read device id", "addr", entry.Name(), "err", err)
			continue
		}

		vendorID := normHex(vendorRaw)
		deviceID := normHex(deviceRaw)

		// Only NVIDIA and AMD GPUs are eligible for VFIO passthrough.
		// Intel iGPUs are the host's primary display and must stay on the host driver.
		if vendorID != "10de" && vendorID != "1002" {
			slog.Debug("gpu discover: skipping non-passthrough vendor", "addr", entry.Name(), "vendor", vendorID)
			continue
		}

		group, err := readIOMMUGroup(devPath)
		if err != nil {
			slog.Debug("gpu discover: IOMMU group unavailable", "addr", entry.Name(), "err", err)
			group = -1
		}

		driver, err := readDriver(devPath)
		if err != nil {
			driver = ""
		}

		gpu := GPUDevice{
			PCIAddress:     entry.Name(),
			Vendor:         vendorFromID(vendorID),
			VendorID:       vendorID,
			DeviceID:       deviceID,
			IOMMUGroup:     group,
			OriginalDriver: driver,
		}

		// Enrich model name and VRAM from the built-in table, then try
		// lspci (PCI ID database) for cards not in the table, then
		// nvidia-smi for precise VRAM on NVIDIA cards.
		gpu.Model, gpu.MemoryMiB = lookupModel(vendorID, deviceID)
		if gpu.Model == "" {
			enrichFromLspci(&gpu)
		}
		if gpu.Model == "" {
			gpu.Model = fmt.Sprintf("Unknown GPU %s:%s", vendorID, deviceID)
		}

		if gpu.Vendor == VendorNVIDIA {
			enrichNVIDIA(&gpu)
		}

		gpus = append(gpus, gpu)
	}

	slog.Debug("gpu discover: found GPUs", "count", len(gpus))
	return gpus, nil
}

// enrichFromLspci attempts to populate Model from the system PCI ID database
// via lspci -vmm. This covers any card known to the database without needing
// vendor-specific tools or a hardcoded table.
func enrichFromLspci(g *GPUDevice) {
	out, err := exec.Command("lspci", "-vmm", "-s", g.PCIAddress).Output()
	if err != nil {
		return
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		name, ok := strings.CutPrefix(line, "Device:\t")
		if ok && strings.TrimSpace(name) != "" {
			g.Model = strings.TrimSpace(name)
			return
		}
	}
}

// enrichNVIDIA attempts to populate Model and MemoryMiB from nvidia-smi.
// Leaves existing values untouched if nvidia-smi is unavailable or fails.
func enrichNVIDIA(gpu *GPUDevice) {
	out, err := exec.Command(
		"nvidia-smi",
		"--query-gpu=gpu_name,memory.total",
		"--format=csv,noheader",
		fmt.Sprintf("--id=%s", gpu.PCIAddress),
	).Output()
	if err != nil {
		return
	}

	line := strings.TrimSpace(string(out))
	parts := strings.SplitN(line, ", ", 2)
	if len(parts) != 2 {
		return
	}

	name := strings.TrimSpace(parts[0])
	memStr := strings.TrimSpace(parts[1])
	memStr = strings.TrimSuffix(memStr, " MiB")
	memMiB, err := strconv.ParseInt(memStr, 10, 64)
	if err != nil {
		return
	}

	gpu.Model = name
	gpu.MemoryMiB = memMiB
}

// isDisplayClass reports whether a PCI class string (e.g. "0x030200") is a
// display controller (class byte 0x03).
func isDisplayClass(class string) bool {
	s := strings.ToLower(strings.TrimSpace(class))
	s = strings.TrimPrefix(s, "0x")
	return len(s) >= 2 && s[:2] == "03"
}

// readIOMMUGroup reads the IOMMU group number from the iommu_group symlink.
// Returns an error if IOMMU is not active (symlink absent).
func readIOMMUGroup(devPath string) (int, error) {
	target, err := os.Readlink(filepath.Join(devPath, "iommu_group"))
	if err != nil {
		return -1, err
	}
	n, err := strconv.Atoi(filepath.Base(target))
	if err != nil {
		return -1, fmt.Errorf("parse iommu group from %q: %w", target, err)
	}
	return n, nil
}

// readDriver returns the basename of the driver currently bound to devPath,
// or "" if no driver is bound.
func readDriver(devPath string) (string, error) {
	target, err := os.Readlink(filepath.Join(devPath, "driver"))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return filepath.Base(target), nil
}

// vendorFromID maps a PCI vendor hex ID to a Vendor constant.
func vendorFromID(vendorID string) Vendor {
	switch strings.ToLower(vendorID) {
	case "10de":
		return VendorNVIDIA
	case "1002":
		return VendorAMD
	case "8086":
		return VendorIntel
	default:
		return VendorUnknown
	}
}

// readSysfsString reads a sysfs attribute file and returns its trimmed content.
func readSysfsString(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// normHex strips the "0x" prefix and lowercases a hex string from sysfs.
func normHex(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.TrimPrefix(s, "0x")
}
