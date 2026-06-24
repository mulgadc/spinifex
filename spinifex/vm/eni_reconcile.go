package vm

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// eni hot-plug QEMU object IDs, derived from the PCIe root-port slot. The
// reconciler and the attach/detach pipelines must agree on these formats so
// query-pci qdev IDs round-trip to a slot.
func eniNetdevID(slot int) string { return fmt.Sprintf("hostnet-eni-%d", slot) }
func eniDeviceID(slot int) string { return fmt.Sprintf("net-eni-%d", slot) }
func eniBusID(slot int) string    { return fmt.Sprintf("hotplug-eni%d", slot) }

// eniSlotFromDeviceID parses a "net-eni-{slot}" qdev ID back to its slot.
// ok is false for any device that is not a hot-plug ENI.
func eniSlotFromDeviceID(qdevID string) (slot int, ok bool) {
	const prefix = "net-eni-"
	if !strings.HasPrefix(qdevID, prefix) {
		return 0, false
	}
	n, err := strconv.Atoi(qdevID[len(prefix):])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// ListLiveENIDevices returns slot → qdev ID for every hot-plug ENI device the
// guest currently exposes via query-pci. Boot-time and non-ENI devices are
// excluded. Returns an error if QMP is unavailable so the reconciler can skip
// the instance rather than treat an empty list as "no devices".
func (m *Manager) ListLiveENIDevices(instance *VM) (map[int]string, error) {
	if instance == nil || instance.QMPClient == nil {
		return nil, fmt.Errorf("%w: instance %v", ErrQMPUnavailable, instanceID(instance))
	}
	dc := newDeviceController(instance)
	devs, err := dc.QueryPCI()
	if err != nil {
		return nil, fmt.Errorf("query-pci: %w", err)
	}
	out := make(map[int]string)
	for _, d := range devs {
		if slot, ok := eniSlotFromDeviceID(d.QDevID); ok {
			out[slot] = d.QDevID
		}
	}
	return out, nil
}

// AdoptENISlot repopulates the in-memory slot map for an ENI the reconciler
// found live (post-restart re-adoption). Idempotent: a slot already mapped to
// eniID is left as-is, and the slot is removed from the free-list at most once.
func (m *Manager) AdoptENISlot(instance *VM, eniID string, slot int) {
	if instance == nil {
		return
	}
	instance.ENIRequests.Mu.Lock()
	defer instance.ENIRequests.Mu.Unlock()
	if instance.ENIRequests.AttachedByENIID == nil {
		instance.ENIRequests.AttachedByENIID = make(map[string]int)
	}
	if cur, ok := instance.ENIRequests.AttachedByENIID[eniID]; ok && cur == slot {
		return
	}
	instance.ENIRequests.AttachedByENIID[eniID] = slot
	filtered := instance.ENIRequests.AvailableSlots[:0]
	for _, s := range instance.ENIRequests.AvailableSlots {
		if s != slot {
			filtered = append(filtered, s)
		}
	}
	instance.ENIRequests.AvailableSlots = filtered
}

// ReleaseENISlot drops eniID from the slot map and returns its slot to the
// free-list. Idempotent: releasing an unknown ENI is a no-op.
func (m *Manager) ReleaseENISlot(instance *VM, eniID string) {
	if instance == nil {
		return
	}
	instance.ENIRequests.Mu.Lock()
	defer instance.ENIRequests.Mu.Unlock()
	slot, ok := instance.ENIRequests.AttachedByENIID[eniID]
	if !ok {
		return
	}
	delete(instance.ENIRequests.AttachedByENIID, eniID)
	instance.ENIRequests.AvailableSlots = append(instance.ENIRequests.AvailableSlots, slot)
}

// RemoveENIDeviceBySlot tears down the QEMU device + netdev for a hot-plug slot
// the reconciler found orphaned (live device, no KV attachment). Best-effort:
// netdev_del failure is logged, not returned, so the slot still frees. The
// caller releases the slot and cleans the tap (when an eniID is known).
func (m *Manager) RemoveENIDeviceBySlot(instance *VM, slot int) error {
	if instance == nil {
		return ErrInstanceNotFound
	}
	if instance.QMPClient == nil {
		return fmt.Errorf("%w: instance %s", ErrQMPUnavailable, instance.ID)
	}
	dc := newDeviceController(instance)
	deviceID := eniDeviceID(slot)
	netdevID := eniNetdevID(slot)
	if err := dc.DeviceDel(deviceID); err != nil {
		return fmt.Errorf("device_del %s: %w", deviceID, err)
	}
	if err := waitForPCIDevice(dc, deviceID, false); err != nil {
		slog.Warn("RemoveENIDeviceBySlot: device removal not observed",
			"instanceId", instance.ID, "slot", slot, "err", err)
	}
	if err := dc.NetdevDel(netdevID); err != nil {
		slog.Warn("RemoveENIDeviceBySlot: netdev_del failed",
			"instanceId", instance.ID, "slot", slot, "err", err)
	}
	return nil
}

// ENISlotForReconcile returns the in-memory slot for eniID, or 0 when the slot
// map does not know it. Used to recover the slot of an attach interrupted
// before HotPlugSlot was persisted to KV.
func (m *Manager) ENISlotForReconcile(instance *VM, eniID string) int {
	if instance == nil {
		return 0
	}
	instance.ENIRequests.Mu.Lock()
	defer instance.ENIRequests.Mu.Unlock()
	return instance.ENIRequests.AttachedByENIID[eniID]
}

// ENISlotMapKeys returns a snapshot of the ENI IDs in the in-memory slot map.
// The reconciler uses it to find slot entries whose KV attachment is gone.
func (m *Manager) ENISlotMapKeys(instance *VM) []string {
	if instance == nil {
		return nil
	}
	instance.ENIRequests.Mu.Lock()
	defer instance.ENIRequests.Mu.Unlock()
	out := make([]string, 0, len(instance.ENIRequests.AttachedByENIID))
	for eniID := range instance.ENIRequests.AttachedByENIID {
		out = append(out, eniID)
	}
	return out
}

// CleanupENITap removes the tap + OVS port for an ENI. Exported wrapper over
// the pipeline's idempotent cleanup so the reconciler can drop tap state when
// finalizing a detached/rolled-back ENI.
func (m *Manager) CleanupENITap(instance *VM, eniID string) {
	if instance == nil {
		return
	}
	m.cleanupENITap(instance.ID, eniID, TapDeviceName(eniID))
}

func instanceID(instance *VM) string {
	if instance == nil {
		return "<nil>"
	}
	return instance.ID
}
