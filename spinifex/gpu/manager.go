package gpu

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

type gpuEntry struct {
	Device        GPUDevice
	InstanceID    string // empty = available
	Available     bool   // false = error state requiring operator action
	groupMembers  []IOMMUGroupMember
	memberDrivers map[string]string // PCI addr -> driver held before VFIO takeover
}

// Manager is a thread-safe pool of passthrough-eligible GPUs.
// It owns the VFIO bind/unbind lifecycle for each claimed device.
type Manager struct {
	mu        sync.Mutex
	pool      []gpuEntry
	sysfsRoot string
}

// NewManager creates a Manager from the given discovered GPUs.
// All entries start as available.
func NewManager(devices []GPUDevice) *Manager {
	pool := make([]gpuEntry, len(devices))
	for i, d := range devices {
		pool[i] = gpuEntry{Device: d, Available: true}
	}
	return &Manager{pool: pool, sysfsRoot: "/sys"}
}

// Claim binds the first available GPU (and all its IOMMU group members) to
// vfio-pci and associates it with instanceID. Returns the claimed device.
func (m *Manager) Claim(instanceID string) (*GPUDevice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	idx := -1
	for i := range m.pool {
		if m.pool[i].Available && m.pool[i].InstanceID == "" {
			idx = i
			break
		}
	}
	if idx == -1 {
		return nil, errors.New("no GPU available")
	}

	entry := &m.pool[idx]
	if entry.Device.IOMMUGroup < 0 {
		return nil, fmt.Errorf("GPU %s has no IOMMU group (is IOMMU enabled?)", entry.Device.PCIAddress)
	}

	members, err := groupMembers(m.sysfsRoot, entry.Device.IOMMUGroup)
	if err != nil {
		return nil, fmt.Errorf("enumerate IOMMU group %d: %w", entry.Device.IOMMUGroup, err)
	}

	drivers := make(map[string]string, len(members))
	var bound []string

	for _, member := range members {
		orig, err := bindVFIO(m.sysfsRoot, member.PCIAddress)
		if err != nil {
			// Rollback any members already bound in this attempt.
			for _, addr := range bound {
				if rbErr := unbindVFIO(m.sysfsRoot, addr, drivers[addr]); rbErr != nil {
					slog.Error("GPU claim rollback failed", "addr", addr, "err", rbErr)
				}
			}
			return nil, fmt.Errorf("bind IOMMU group member %s: %w", member.PCIAddress, err)
		}
		// bindVFIO returns "vfio-pci" when the device was already bound (idempotent
		// path — e.g. pre-bound by spx-test-gpu, or after a daemon restart with a
		// live GPU instance). Preserve "vfio-pci" as the recorded original so
		// Release knows to skip the unbind/rebind cycle for these devices.
		if orig == "vfio-pci" {
			if member.PCIAddress == entry.Device.PCIAddress {
				orig = entry.Device.OriginalDriver
			}
			// else: companion was already vfio-pci bound; keep orig="vfio-pci"
			// so Release leaves it bound rather than unbinding without a rebind target.
		}
		drivers[member.PCIAddress] = orig
		bound = append(bound, member.PCIAddress)
	}

	entry.InstanceID = instanceID
	entry.groupMembers = members
	entry.memberDrivers = drivers

	slog.Info("GPU claimed", "instance", instanceID,
		"gpu", entry.Device.PCIAddress, "model", entry.Device.Model)
	return &entry.Device, nil
}

// Release returns the GPU held by instanceID to the available pool.
// For devices that were already bound to vfio-pci before the daemon started
// (e.g. by spx-test-gpu), the device is left vfio-pci bound — QEMU has
// already released the group fd on exit, so the next Claim works immediately.
// For devices that were bound by Claim itself, Release unbinds vfio-pci and
// rebinds to the original driver; if that fails the entry is marked unavailable.
func (m *Manager) Release(instanceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	idx := -1
	for i := range m.pool {
		if m.pool[i].InstanceID == instanceID {
			idx = i
			break
		}
	}
	if idx == -1 {
		return fmt.Errorf("no GPU claimed by instance %s", instanceID)
	}

	entry := &m.pool[idx]
	var firstErr error

	for _, member := range entry.groupMembers {
		orig := entry.memberDrivers[member.PCIAddress]
		if orig == "vfio-pci" {
			// GPU was already bound to vfio-pci before this instance (e.g. by
			// spx-test-gpu at node setup). QEMU released the group fd on exit;
			// the device stays vfio-pci bound and is immediately available for
			// the next claim. Skipping unbind avoids a needless round-trip and
			// eliminates a brief window where the device is unbound.
			continue
		}
		if err := unbindVFIO(m.sysfsRoot, member.PCIAddress, orig); err != nil {
			slog.Error("GPU release failed for IOMMU group member",
				"instance", instanceID, "pci", member.PCIAddress, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	entry.InstanceID = ""
	entry.groupMembers = nil
	entry.memberDrivers = nil

	if firstErr != nil {
		entry.Available = false
		slog.Error("GPU marked unavailable after failed release — operator action required",
			"gpu", entry.Device.PCIAddress)
		return firstErr
	}

	slog.Info("GPU released", "instance", instanceID, "gpu", entry.Device.PCIAddress)
	return nil
}

// Available returns the count of GPUs that can be claimed right now.
func (m *Manager) Available() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, e := range m.pool {
		if e.Available && e.InstanceID == "" {
			n++
		}
	}
	return n
}

// AllocatedCount returns the number of GPUs currently claimed by instances.
func (m *Manager) AllocatedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, e := range m.pool {
		if e.InstanceID != "" {
			n++
		}
	}
	return n
}

// TotalCount returns the total number of GPUs in the pool regardless of state.
func (m *Manager) TotalCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pool)
}

// ReclaimByAddress marks the GPU at addr as claimed by instanceID without
// performing sysfs writes. Used on daemon restart to re-register GPUs that
// are still bound to vfio-pci from a previous run.
func (m *Manager) ReclaimByAddress(addr, instanceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := range m.pool {
		if m.pool[i].Device.PCIAddress != addr {
			continue
		}
		entry := &m.pool[i]
		if entry.InstanceID == instanceID {
			// Already owned by the same instance — OnInstanceUp can fire
			// for a fresh launch where the handler already Claim'd this
			// slot. Treat as a no-op rather than a conflict.
			return nil
		}
		if entry.InstanceID != "" {
			return fmt.Errorf("GPU %s already claimed by %s", addr, entry.InstanceID)
		}

		members, err := groupMembers(m.sysfsRoot, entry.Device.IOMMUGroup)
		if err != nil {
			slog.Warn("Failed to re-discover IOMMU group on restart, teardown may be incomplete",
				"gpu", addr, "group", entry.Device.IOMMUGroup, "err", err)
			members = []IOMMUGroupMember{{PCIAddress: addr}}
		}

		// Use the stored pre-passthrough driver for the primary GPU so Release
		// can rebind correctly. Companion devices are left unbound on release
		// (original drivers not persisted across restarts).
		drivers := make(map[string]string, len(members))
		for _, member := range members {
			if member.PCIAddress == addr {
				drivers[member.PCIAddress] = entry.Device.OriginalDriver
			}
		}

		entry.InstanceID = instanceID
		entry.groupMembers = members
		entry.memberDrivers = drivers
		slog.Info("GPU re-claimed after daemon restart", "gpu", addr, "instance", instanceID)
		return nil
	}
	return fmt.Errorf("GPU %s not found in pool", addr)
}
