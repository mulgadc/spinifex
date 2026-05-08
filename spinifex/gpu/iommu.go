package gpu

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// GroupMembers returns all PCI devices in the given IOMMU group.
// All members must be bound to vfio-pci together for passthrough.
func GroupMembers(groupNum int) ([]IOMMUGroupMember, error) {
	return groupMembers("/sys", groupNum)
}

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
