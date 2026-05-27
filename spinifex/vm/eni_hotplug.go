package vm

import (
	"fmt"
	"log/slog"
	"time"
)

// eniPipelineSettings governs the query-pci polling cadence of the
// HotPlugENI / HotUnplugENI pipelines. Overridable for tests via the
// package-level seam below.
type eniPipelineSettings struct {
	AttachPollInterval time.Duration
	AttachPollMax      int
	DetachPollInterval time.Duration
	DetachPollMaxSoft  int // !force timeout = interval * max
	DetachPollMaxForce int // force=true short-circuits after this many polls
}

var eniPipeline = eniPipelineSettings{
	AttachPollInterval: 200 * time.Millisecond,
	AttachPollMax:      25, // 5s total
	DetachPollInterval: 200 * time.Millisecond,
	DetachPollMaxSoft:  25, // 5s for graceful detach
	DetachPollMaxForce: 5,  // 1s for force=true
}

// newDeviceController is the production factory bound to the live
// QMPClient on the VM. Tests override this seam to inject a
// StubDeviceController without touching the VM's QMP socket.
var newDeviceController = func(v *VM) DeviceController {
	return NewQMPDeviceController(v.QMPClient, v.ID)
}

// HotPlugENIResult carries the slot index assigned to a successful
// hot-plug. Returned to the daemon handler so the AttachmentId KV record
// can be annotated with HotPlugSlot before the NATS success event fires.
type HotPlugENIResult struct {
	Slot int
}

// HotPlugENI runs the 4-step attach pipeline against the live VM:
//
//  1. tap device creation     (placeholder — wired in Sprint 3c)
//  2. ovs-vsctl add-port      (placeholder — wired in Sprint 3c)
//  3. QMP netdev_add          (real)
//  4. QMP device_add + poll   (real)
//
// On step-N failure, steps 1..N-1 are rolled back in reverse order and
// the slot is returned to ENIRequests.AvailableSlots. The instance must
// be running and have a live QMPClient.
//
// Caller (daemon handler) owns the KV side-effects: marking
// AttachmentStatus=attached and publishing the
// vpc.eni-hotplug.attached.{instanceID} event happens after this returns
// nil.
func (m *Manager) HotPlugENI(instance *VM, eniID, mac string) (HotPlugENIResult, error) {
	if instance == nil {
		return HotPlugENIResult{}, ErrInstanceNotFound
	}
	if status := m.Status(instance); status != StateRunning {
		return HotPlugENIResult{}, fmt.Errorf("%w: cannot hot-plug ENI on instance %s in state %s",
			ErrInvalidTransition, instance.ID, status)
	}
	if instance.QMPClient == nil {
		return HotPlugENIResult{}, fmt.Errorf("%w: instance %s", ErrQMPUnavailable, instance.ID)
	}

	instance.ENIRequests.Mu.Lock()
	defer instance.ENIRequests.Mu.Unlock()

	if existing, ok := instance.ENIRequests.AttachedByENIID[eniID]; ok {
		// Idempotent re-attach against the same instance is a noop —
		// returning the existing slot lets the handler treat the call as
		// a successful state confirmation.
		return HotPlugENIResult{Slot: existing}, nil
	}

	if len(instance.ENIRequests.AvailableSlots) == 0 {
		return HotPlugENIResult{}, ErrAttachmentLimitExceeded
	}
	slot := instance.ENIRequests.AvailableSlots[0]
	instance.ENIRequests.AvailableSlots = instance.ENIRequests.AvailableSlots[1:]
	if instance.ENIRequests.AttachedByENIID == nil {
		instance.ENIRequests.AttachedByENIID = make(map[string]int)
	}
	instance.ENIRequests.AttachedByENIID[eniID] = slot

	dc := newDeviceController(instance)
	netdevID := fmt.Sprintf("hostnet-eni-%d", slot)
	deviceID := fmt.Sprintf("net-eni-%d", slot)
	tapName := TapDeviceName(eniID)
	busID := fmt.Sprintf("hotplug-eni%d", slot)

	// Step 1: tap device. Sprint 3c replaces this stub with a real call
	// to NetworkPlumber (ip tuntap add).
	if err := stubTapAdd(instance.ID, eniID, tapName); err != nil {
		m.releaseSlotLocked(instance, eniID, slot)
		return HotPlugENIResult{}, fmt.Errorf("tap add (stub): %w", err)
	}

	// Step 2: OVS port. Sprint 3c replaces this stub with the real
	// ovs-vsctl add-port invocation against br-int.
	if err := stubOVSAddPort(instance.ID, eniID, tapName, mac); err != nil {
		_ = stubTapDel(instance.ID, eniID, tapName)
		m.releaseSlotLocked(instance, eniID, slot)
		return HotPlugENIResult{}, fmt.Errorf("ovs add-port (stub): %w", err)
	}

	// Step 3: QMP netdev_add.
	if err := dc.NetdevAdd(map[string]any{
		"type":       "tap",
		"id":         netdevID,
		"ifname":     tapName,
		"script":     "no",
		"downscript": "no",
	}); err != nil {
		_ = stubOVSDelPort(instance.ID, eniID, tapName)
		_ = stubTapDel(instance.ID, eniID, tapName)
		m.releaseSlotLocked(instance, eniID, slot)
		return HotPlugENIResult{}, fmt.Errorf("QMP netdev_add: %w", err)
	}

	// Step 4: QMP device_add then poll query-pci until materialized.
	if err := dc.DeviceAdd(map[string]any{
		"driver": "virtio-net-pci",
		"id":     deviceID,
		"bus":    busID,
		"netdev": netdevID,
		"mac":    mac,
	}); err != nil {
		_ = dc.NetdevDel(netdevID)
		_ = stubOVSDelPort(instance.ID, eniID, tapName)
		_ = stubTapDel(instance.ID, eniID, tapName)
		m.releaseSlotLocked(instance, eniID, slot)
		return HotPlugENIResult{}, fmt.Errorf("QMP device_add: %w", err)
	}

	if err := waitForPCIDevice(dc, deviceID, true); err != nil {
		_ = dc.DeviceDel(deviceID)
		_ = dc.NetdevDel(netdevID)
		_ = stubOVSDelPort(instance.ID, eniID, tapName)
		_ = stubTapDel(instance.ID, eniID, tapName)
		m.releaseSlotLocked(instance, eniID, slot)
		return HotPlugENIResult{}, fmt.Errorf("query-pci wait (attach): %w", err)
	}

	slog.Info("ENI hot-plugged",
		"instanceId", instance.ID, "eniId", eniID, "slot", slot,
		"deviceId", deviceID, "netdevId", netdevID, "tap", tapName)
	return HotPlugENIResult{Slot: slot}, nil
}

// HotUnplugENI runs the reverse 4-step detach pipeline. force=true
// shortens the post-device_del poll budget so an unresponsive guest does
// not block the call — at the cost of leaving the guest with a dangling
// virtio-net device until reboot if the kernel never releases it.
func (m *Manager) HotUnplugENI(instance *VM, eniID string, force bool) error {
	if instance == nil {
		return ErrInstanceNotFound
	}
	if status := m.Status(instance); status != StateRunning {
		return fmt.Errorf("%w: cannot hot-unplug ENI from instance %s in state %s",
			ErrInvalidTransition, instance.ID, status)
	}
	if instance.QMPClient == nil {
		return fmt.Errorf("%w: instance %s", ErrQMPUnavailable, instance.ID)
	}

	instance.ENIRequests.Mu.Lock()
	defer instance.ENIRequests.Mu.Unlock()

	slot, ok := instance.ENIRequests.AttachedByENIID[eniID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrENINotAttached, eniID)
	}

	dc := newDeviceController(instance)
	netdevID := fmt.Sprintf("hostnet-eni-%d", slot)
	deviceID := fmt.Sprintf("net-eni-%d", slot)
	tapName := TapDeviceName(eniID)

	// Step 1: device_del (guest driver release request).
	if err := dc.DeviceDel(deviceID); err != nil {
		if !force {
			return fmt.Errorf("QMP device_del: %w", err)
		}
		slog.Warn("HotUnplugENI: device_del failed (force=true, continuing)",
			"instanceId", instance.ID, "eniId", eniID, "err", err)
	}

	// Step 2: poll query-pci for absence. force=true uses a shorter budget.
	if err := waitForPCIDevice(dc, deviceID, false); err != nil {
		if !force {
			return fmt.Errorf("query-pci wait (detach): %w", err)
		}
		slog.Warn("HotUnplugENI: device removal not observed (force=true, continuing)",
			"instanceId", instance.ID, "eniId", eniID, "err", err)
	}

	// Step 3: QMP netdev_del.
	if err := dc.NetdevDel(netdevID); err != nil {
		slog.Warn("HotUnplugENI: netdev_del failed (continuing detach)",
			"instanceId", instance.ID, "eniId", eniID, "err", err)
	}

	// Step 4: OVS port + tap (stubs, real wiring in Sprint 3c).
	_ = stubOVSDelPort(instance.ID, eniID, tapName)
	_ = stubTapDel(instance.ID, eniID, tapName)

	delete(instance.ENIRequests.AttachedByENIID, eniID)
	instance.ENIRequests.AvailableSlots = append(instance.ENIRequests.AvailableSlots, slot)

	slog.Info("ENI hot-unplugged",
		"instanceId", instance.ID, "eniId", eniID, "slot", slot, "force", force)
	return nil
}

// releaseSlotLocked returns slot to the free-list and clears the
// AttachedByENIID entry. Caller must hold instance.ENIRequests.Mu.
func (m *Manager) releaseSlotLocked(instance *VM, eniID string, slot int) {
	delete(instance.ENIRequests.AttachedByENIID, eniID)
	instance.ENIRequests.AvailableSlots = append(instance.ENIRequests.AvailableSlots, slot)
}

// waitForPCIDevice polls QueryPCI until presence (wantPresent=true) or
// absence (wantPresent=false) of deviceID is observed. Attach and detach
// use distinct poll budgets via the package-level eniPipeline settings.
func waitForPCIDevice(dc DeviceController, deviceID string, wantPresent bool) error {
	interval := eniPipeline.AttachPollInterval
	maxAttempts := eniPipeline.AttachPollMax
	if !wantPresent {
		interval = eniPipeline.DetachPollInterval
		maxAttempts = eniPipeline.DetachPollMaxSoft
	}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		devs, err := dc.QueryPCI()
		if err != nil {
			return fmt.Errorf("query-pci attempt %d: %w", attempt, err)
		}
		found := false
		for _, d := range devs {
			if d.QDevID == deviceID {
				found = true
				break
			}
		}
		if found == wantPresent {
			return nil
		}
		if attempt < maxAttempts {
			eniPipelineSleep(interval)
		}
	}
	return fmt.Errorf("%w: device %s want_present=%v", ErrENIPipelineTimeout, deviceID, wantPresent)
}

// eniPipelineSleep is the sleep seam used by waitForPCIDevice. Tests
// override it to drive poll cadence without burning wall-clock time.
var eniPipelineSleep = time.Sleep

// SetHotPlugTestSeams swaps the device-controller factory and the
// query-pci poll sleep for the duration of a test, returning a restore
// func that callers pass to t.Cleanup. Mirrors utils.SetSudoCommandForTest:
// the indirection lets tests in other packages drive the hot-plug
// pipeline against a StubDeviceController without reassigning
// unexported vars (which the reassign linter forbids). Pass nil for
// either argument to leave the corresponding seam untouched.
func SetHotPlugTestSeams(factory func(*VM) DeviceController, sleep func(time.Duration)) (restore func()) {
	origFactory := newDeviceController
	origSleep := eniPipelineSleep
	if factory != nil {
		newDeviceController = factory
	}
	if sleep != nil {
		eniPipelineSleep = sleep
	}
	return func() {
		newDeviceController = origFactory
		eniPipelineSleep = origSleep
	}
}

// stubTapAdd / stubTapDel / stubOVSAddPort / stubOVSDelPort emit a debug
// log and return nil. Sprint 3c replaces these with real
// NetworkPlumber.SetupTap / TeardownTap + ovs-vsctl invocations. The
// pipeline structure and rollback semantics are stable in 3b so 3c is a
// drop-in replacement.

func stubTapAdd(instanceID, eniID, tapName string) error {
	slog.Debug("ENI hot-plug step 1 (tap add) — stubbed in Sprint 3b",
		"instanceId", instanceID, "eniId", eniID, "tap", tapName)
	return nil
}

func stubTapDel(instanceID, eniID, tapName string) error {
	slog.Debug("ENI hot-plug rollback (tap del) — stubbed in Sprint 3b",
		"instanceId", instanceID, "eniId", eniID, "tap", tapName)
	return nil
}

func stubOVSAddPort(instanceID, eniID, tapName, mac string) error {
	slog.Debug("ENI hot-plug step 2 (ovs add-port) — stubbed in Sprint 3b",
		"instanceId", instanceID, "eniId", eniID, "tap", tapName, "mac", mac)
	return nil
}

func stubOVSDelPort(instanceID, eniID, tapName string) error {
	slog.Debug("ENI hot-plug rollback (ovs del-port) — stubbed in Sprint 3b",
		"instanceId", instanceID, "eniId", eniID, "tap", tapName)
	return nil
}
