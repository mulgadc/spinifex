package gpu

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// groupMembers returns all PCI devices in the given IOMMU group.
// All members must be bound to vfio-pci together for passthrough.
func groupMembers(sysfsRoot string, groupNum int) ([]IOMMUGroupMember, error) {
	devicesPath := filepath.Join(sysfsRoot, "kernel/iommu_groups", strconv.Itoa(groupNum), "devices")
	entries, err := os.ReadDir(devicesPath)
	if err != nil {
		return nil, fmt.Errorf("read iommu group %d: %w", groupNum, err)
	}

	members := make([]IOMMUGroupMember, 0, len(entries))
	for _, entry := range entries {
		pciAddr := entry.Name()
		// Read device attributes from the canonical pci devices path to avoid
		// chasing relative symlinks inside the iommu_groups tree.
		devPath := filepath.Join(sysfsRoot, "bus/pci/devices", pciAddr)

		member := IOMMUGroupMember{PCIAddress: pciAddr}

		if v, err := readSysfsString(filepath.Join(devPath, "vendor")); err == nil {
			member.VendorID = normHex(v)
		}
		if d, err := readSysfsString(filepath.Join(devPath, "device")); err == nil {
			member.DeviceID = normHex(d)
		}
		if c, err := readSysfsString(filepath.Join(devPath, "class")); err == nil {
			member.Class = c
		}

		members = append(members, member)
	}

	return members, nil
}

// isBridgeClass reports whether a PCI class string identifies a bridge device
// (base class 0x06 — host, ISA, PCI-to-PCI, etc). A passthrough endpoint's
// IOMMU group can include an upstream bridge when the platform lacks ACS
// isolation between them; the bridge itself never performs DMA and vfio-pci
// refuses to bind non-endpoint devices, so it must stay off the VFIO
// bind/unbind lifecycle while the endpoint is claimed.
func isBridgeClass(class string) bool {
	s := strings.ToLower(strings.TrimSpace(class))
	s = strings.TrimPrefix(s, "0x")
	return len(s) >= 2 && s[:2] == "06"
}

// filterBridgeMembers returns members with bridge-class devices removed.
func filterBridgeMembers(members []IOMMUGroupMember) []IOMMUGroupMember {
	out := make([]IOMMUGroupMember, 0, len(members))
	for _, m := range members {
		if !isBridgeClass(m.Class) {
			out = append(out, m)
		}
	}
	return out
}
