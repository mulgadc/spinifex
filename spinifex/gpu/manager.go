package gpu

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
)

type gpuEntry struct {
	Device        GPUDevice
	MIGInstance   *MIGInstance       // nil = whole-GPU passthrough; non-nil = MIG slice
	InstanceID    string             // empty = available
	Available     bool               // false = error state requiring operator action
	groupMembers  []IOMMUGroupMember // only populated for whole-GPU entries
	memberDrivers map[string]string  // PCI addr -> driver held before VFIO takeover; whole-GPU only
}

// Manager is a thread-safe pool of passthrough-eligible GPUs.
// It owns the VFIO bind/unbind lifecycle for each claimed device.
type Manager struct {
	mu                sync.Mutex
	pool              []gpuEntry
	sysfsRoot         string
	freeMIGGPUs       []GPUDevice            // MIG-capable GPUs not yet committed to a profile
	committedProfiles map[string]*MIGProfile // pciAddr → active profile (dynamic claims only)
}

// NewManager creates a Manager from the given discovered GPUs.
// All entries start as available.
func NewManager(devices []GPUDevice) *Manager {
	pool := make([]gpuEntry, len(devices))
	for i, d := range devices {
		pool[i] = gpuEntry{Device: d, Available: true}
	}
	return &Manager{
		pool:              pool,
		sysfsRoot:         "/sys",
		committedProfiles: make(map[string]*MIGProfile),
	}
}

// AddMIGGPU registers a MIG-capable GPU as available for dynamic profile
// allocation. No pool entries are created until the first Claim for this GPU.
// Used for fresh daemon starts where no MIG instances exist yet.
func (m *Manager) AddMIGGPU(dev GPUDevice) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.freeMIGGPUs = append(m.freeMIGGPUs, dev)
}

// Claim associates the first available GPU with instanceID. profileName selects
// a MIG profile (e.g. "1g.10gb") or "" for whole-GPU passthrough. A pre-carved
// slice is claimed immediately; otherwise a free MIG GPU is carved on-demand.
// The returned *MIGInstance is non-nil only for MIG entries.
func (m *Manager) Claim(instanceID, profileName string) (*GPUDevice, *MIGInstance, error) {
	m.mu.Lock()

	if profileName != "" {
		// --- MIG FAST PATH: claim a pre-carved slice ---
		for i := range m.pool {
			e := &m.pool[i]
			if e.MIGInstance == nil || !e.Available || e.InstanceID != "" {
				continue
			}
			if e.MIGInstance.Profile.Name != profileName {
				continue
			}
			if _, err := os.Stat(e.MIGInstance.MdevPath); err != nil {
				m.mu.Unlock()
				return nil, nil, fmt.Errorf("MIG mdev %s not accessible: %w", e.MIGInstance.MdevPath, err)
			}
			e.InstanceID = instanceID
			dev, mig := e.Device, e.MIGInstance
			m.mu.Unlock()
			slog.Info("MIG instance claimed", "instance", instanceID,
				"gpu", dev.PCIAddress, "profile", mig.Profile.Name, "mdev", mig.MdevPath)
			return &dev, mig, nil
		}

		// --- MIG SLOW PATH: carve a free GPU ---
		if len(m.freeMIGGPUs) == 0 {
			m.mu.Unlock()
			return nil, nil, fmt.Errorf("no MIG GPU available for profile %q", profileName)
		}
		dev := m.freeMIGGPUs[0]
		m.freeMIGGPUs = m.freeMIGGPUs[1:]
		m.mu.Unlock()

		profiles, err := ListProfiles(dev.PCIAddress)
		if err != nil {
			m.mu.Lock()
			m.freeMIGGPUs = append([]GPUDevice{dev}, m.freeMIGGPUs...)
			m.mu.Unlock()
			return nil, nil, fmt.Errorf("list MIG profiles on %s: %w", dev.PCIAddress, err)
		}
		var chosen *MIGProfile
		for _, p := range profiles {
			if p.Name == profileName {
				pp := p
				chosen = &pp
				break
			}
		}
		if chosen == nil {
			m.mu.Lock()
			m.freeMIGGPUs = append([]GPUDevice{dev}, m.freeMIGGPUs...)
			m.mu.Unlock()
			return nil, nil, fmt.Errorf("MIG profile %q not supported on %s", profileName, dev.PCIAddress)
		}

		instances, err := CreateInstances(dev.PCIAddress, *chosen)
		if err != nil {
			m.mu.Lock()
			m.freeMIGGPUs = append([]GPUDevice{dev}, m.freeMIGGPUs...)
			m.mu.Unlock()
			return nil, nil, fmt.Errorf("create MIG instances on %s: %w", dev.PCIAddress, err)
		}

		m.mu.Lock()
		for i := range instances {
			m.pool = append(m.pool, gpuEntry{
				Device:      dev,
				MIGInstance: &instances[i],
				Available:   true,
			})
		}
		m.committedProfiles[dev.PCIAddress] = chosen
		for i := range m.pool {
			e := &m.pool[i]
			if e.Device.PCIAddress == dev.PCIAddress && e.MIGInstance != nil && e.InstanceID == "" {
				e.InstanceID = instanceID
				retDev, retMIG := e.Device, e.MIGInstance
				m.mu.Unlock()
				slog.Info("MIG instance claimed from freshly carved GPU", "instance", instanceID,
					"gpu", dev.PCIAddress, "profile", profileName, "mdev", retMIG.MdevPath)
				return &retDev, retMIG, nil
			}
		}
		m.mu.Unlock()
		return nil, nil, errors.New("internal: no MIG slice available after carving")
	}

	// --- WHOLE-GPU PATH (unchanged) ---
	idx := -1
	for i := range m.pool {
		if m.pool[i].Available && m.pool[i].InstanceID == "" && m.pool[i].MIGInstance == nil {
			idx = i
			break
		}
	}
	if idx == -1 {
		m.mu.Unlock()
		return nil, nil, errors.New("no GPU available")
	}

	entry := &m.pool[idx]

	if entry.Device.IOMMUGroup < 0 {
		m.mu.Unlock()
		return nil, nil, fmt.Errorf("GPU %s has no IOMMU group (is IOMMU enabled?)", entry.Device.PCIAddress)
	}

	members, err := groupMembers(m.sysfsRoot, entry.Device.IOMMUGroup)
	if err != nil {
		m.mu.Unlock()
		return nil, nil, fmt.Errorf("enumerate IOMMU group %d: %w", entry.Device.IOMMUGroup, err)
	}
	members = filterBridgeMembers(members)

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
			m.mu.Unlock()
			return nil, nil, fmt.Errorf("bind IOMMU group member %s: %w", member.PCIAddress, err)
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
	m.mu.Unlock()
	return &entry.Device, nil, nil
}

// Release returns the GPU held by instanceID to the pool. Pre-bound devices
// (already vfio-pci before the daemon) are left bound; Claim-bound devices are
// unbound and rebind to their original driver (failure marks them unavailable).
// For dynamically carved MIG GPUs, the last release destroys all slices.
func (m *Manager) Release(instanceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var firstErr error
	released := false
	var releasedMIGPCI string

	for i := range m.pool {
		if m.pool[i].InstanceID != instanceID {
			continue
		}
		released = true
		entry := &m.pool[i]

		if entry.MIGInstance != nil {
			releasedMIGPCI = entry.Device.PCIAddress
			entry.InstanceID = ""
			slog.Info("MIG instance released", "instance", instanceID,
				"gpu", entry.Device.PCIAddress, "mdev", entry.MIGInstance.MdevPath)
			continue
		}

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
		} else {
			slog.Info("GPU released", "instance", instanceID, "gpu", entry.Device.PCIAddress)
		}
	}

	if !released {
		return fmt.Errorf("no GPU claimed by instance %s", instanceID)
	}

	// For dynamically carved MIG GPUs: destroy all slices when the last instance
	// on this GPU is released, returning the GPU to the free pool.
	if releasedMIGPCI != "" {
		if _, dynamic := m.committedProfiles[releasedMIGPCI]; dynamic {
			m.tryDestroyIdleMIGGPULocked(releasedMIGPCI)
		}
	}

	return firstErr
}

// tryDestroyIdleMIGGPULocked destroys MIG instances on pciAddr when none are
// claimed and returns the GPU to freeMIGGPUs. Must be called with m.mu held.
func (m *Manager) tryDestroyIdleMIGGPULocked(pciAddr string) {
	for _, e := range m.pool {
		if e.Device.PCIAddress == pciAddr && e.MIGInstance != nil && e.InstanceID != "" {
			return // still active instances on this GPU
		}
	}

	var dev GPUDevice
	found := false
	for _, e := range m.pool {
		if e.Device.PCIAddress == pciAddr && e.MIGInstance != nil {
			dev = e.Device
			found = true
			break
		}
	}
	if !found {
		return
	}

	m.mu.Unlock()
	err := DestroyAllInstances(pciAddr)
	m.mu.Lock()

	if err != nil {
		slog.Error("MIG destroy failed on last release; GPU marked unavailable — restart daemon to recover",
			"gpu", pciAddr, "err", err)
		for i := range m.pool {
			if m.pool[i].Device.PCIAddress == pciAddr && m.pool[i].MIGInstance != nil {
				m.pool[i].Available = false
			}
		}
		delete(m.committedProfiles, pciAddr)
		return
	}

	newPool := make([]gpuEntry, 0, len(m.pool))
	for _, e := range m.pool {
		if e.Device.PCIAddress != pciAddr || e.MIGInstance == nil {
			newPool = append(newPool, e)
		}
	}
	m.pool = newPool
	delete(m.committedProfiles, pciAddr)
	m.freeMIGGPUs = append(m.freeMIGGPUs, dev)
	slog.Info("MIG GPU freed and returned to pool", "gpu", pciAddr)
}

// MarkFailed marks a whole-GPU entry as permanently unavailable after a
// VFIO/QEMU launch error. Claim skips it until the daemon restarts.
func (m *Manager) MarkFailed(pciAddress string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.pool {
		e := &m.pool[i]
		if e.Device.PCIAddress == pciAddress && e.MIGInstance == nil && e.InstanceID == "" {
			e.Available = false
			slog.Warn("GPU marked unavailable after launch failure — restart daemon to retry",
				"pci", pciAddress)
			return
		}
	}
}

// MarkMIGFailed marks a MIG slice unavailable after a QEMU launch error.
// Other slices on the same GPU remain available.
func (m *Manager) MarkMIGFailed(mdevPath string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.pool {
		e := &m.pool[i]
		if e.MIGInstance != nil && e.MIGInstance.MdevPath == mdevPath && e.InstanceID == "" {
			e.Available = false
			slog.Warn("MIG instance marked unavailable after launch failure — restart daemon to retry",
				"mdev", mdevPath)
			return
		}
	}
}

// AddMIGInstances appends pool entries for each MIG slice on device.
func (m *Manager) AddMIGInstances(device GPUDevice, instances []MIGInstance) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range instances {
		m.pool = append(m.pool, gpuEntry{
			Device:      device,
			MIGInstance: &instances[i],
			Available:   true,
		})
	}
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

// PoolEntry is a read-only snapshot of a single pool slot, returned by Snapshot.
type PoolEntry struct {
	Device      GPUDevice
	MIGInstance *MIGInstance // nil for whole-GPU entries
	InstanceID  string       // empty if free
	Available   bool
}

// Snapshot returns a point-in-time copy of every pool slot including free MIG GPUs.
// The caller receives owned copies and may inspect them without synchronisation.
func (m *Manager) Snapshot() []PoolEntry {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]PoolEntry, 0, len(m.pool)+len(m.freeMIGGPUs))
	for _, e := range m.pool {
		entry := PoolEntry{
			Device:     e.Device,
			InstanceID: e.InstanceID,
			Available:  e.Available,
		}
		if e.MIGInstance != nil {
			mig := *e.MIGInstance
			entry.MIGInstance = &mig
		}
		out = append(out, entry)
	}
	// Free MIG GPUs are MIG-capable but not yet carved into slices.
	for _, dev := range m.freeMIGGPUs {
		out = append(out, PoolEntry{Device: dev, Available: true})
	}
	return out
}

// ReclaimByAddress marks the GPU at addr as claimed without sysfs writes.
// Used on daemon restart for GPUs still bound to vfio-pci from a previous run.
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
		members = filterBridgeMembers(members)

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

// ReclaimByMdev marks the MIG slice at mdevPath as claimed without sysfs writes.
// Used on daemon restart for slices whose VMs are still running.
func (m *Manager) ReclaimByMdev(mdevPath, instanceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := range m.pool {
		e := &m.pool[i]
		if e.MIGInstance == nil || e.MIGInstance.MdevPath != mdevPath {
			continue
		}
		if e.InstanceID == instanceID {
			// Same instance re-registering — idempotent.
			return nil
		}
		if e.InstanceID != "" {
			return fmt.Errorf("MIG instance %s already claimed by %s", mdevPath, e.InstanceID)
		}
		if _, err := os.Stat(mdevPath); err != nil {
			return fmt.Errorf("MIG mdev %s not accessible (MIG mode may have been disabled): %w", mdevPath, err)
		}
		e.InstanceID = instanceID
		slog.Info("MIG instance re-claimed after daemon restart", "mdev", mdevPath, "instance", instanceID)
		return nil
	}
	return fmt.Errorf("MIG instance %s not found in pool", mdevPath)
}
