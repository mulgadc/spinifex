package vm

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/qmp"
)

// pciAddrRegexp extracts the device index from a boot-time QDev path.
// Example: "/machine/peripheral-anon/device[0]/virtio-backend" → 0
var pciAddrRegexp = regexp.MustCompile(`device\[(\d+)\]`)

// hotplugPortRegexp extracts the EBS hot-plug port number from a QDev path.
// Example: "/machine/peripheral/vdisk-vol-xxx/hotplug-ebs3/virtio-backend" or
//
//	"/machine/peripheral/vdisk-vol-xxx/virtio-backend" (bus assigned by QEMU)
//
// The chassis number from the QEMU command line determines PCI slot order.
// ENI hot-plug ports (`hotplug-eni{N}`) are explicitly excluded — virtio-net
// devices are not surfaced through query-block, but the explicit suffix
// guards against future query callers conflating the two pools.
var hotplugPortRegexp = regexp.MustCompile(`hotplug-ebs(\d+)`)

// queryGuestDeviceMap uses QMP query-block to build a map from QEMU device ID
// (e.g. "os", "cloudinit", "vdisk-vol-xxx") to the guest device path
// (e.g. "/dev/vda", "/dev/vdb", "/dev/vdc").
//
// The mapping is derived from PCI address order: virtio-blk-pci devices are
// enumerated by the guest kernel in PCI bus order, which corresponds to the
// device index in the QDev path.
func queryGuestDeviceMap(qmpClient *qmp.QMPClient, instanceID string) (map[string]string, error) {
	resp, err := sendQMPCommand(qmpClient, qmp.QMPCommand{Execute: "query-block"}, instanceID)
	if err != nil {
		return nil, fmt.Errorf("query-block failed: %w", err)
	}

	var devices []qmp.BlockDevice
	if err := json.Unmarshal(resp.Return, &devices); err != nil {
		return nil, fmt.Errorf("failed to parse query-block response: %w", err)
	}

	return buildDeviceMap(devices), nil
}

// deviceMapRetrySleep is the sleep seam used by queryGuestDeviceMapWait.
// Tests override it to drive the retry cadence without burning real
// wall-clock time on each iteration.
var deviceMapRetrySleep = time.Sleep

// queryGuestDeviceMapWait retries queryGuestDeviceMap until expectedDevice
// appears in the result. This handles the race where query-block is called
// immediately after device_add, before QEMU has registered the new device.
func queryGuestDeviceMapWait(qmpClient *qmp.QMPClient, instanceID, expectedDevice string) (map[string]string, error) {
	const maxAttempts = 5
	const retryDelay = 200 * time.Millisecond

	for attempt := range maxAttempts {
		deviceMap, err := queryGuestDeviceMap(qmpClient, instanceID)
		if err != nil {
			return nil, err
		}
		if _, ok := deviceMap[expectedDevice]; ok {
			return deviceMap, nil
		}
		if attempt < maxAttempts-1 {
			slog.Debug("queryGuestDeviceMapWait: device not yet visible, retrying",
				"expectedDevice", expectedDevice, "attempt", attempt+1, "maxAttempts", maxAttempts)
			deviceMapRetrySleep(retryDelay)
		}
	}

	// Return the last result even though the device wasn't found — caller decides fallback.
	return queryGuestDeviceMap(qmpClient, instanceID)
}

// buildDeviceMap takes a list of BlockDevices from QMP and returns a map
// from QEMU device ID to guest /dev/vdX path, sorted by PCI address.
//
// Two QDev path formats exist:
//   - Boot-time:  /machine/peripheral-anon/device[N]/virtio-backend
//     device field contains the ID (e.g. "os", "cloudinit")
//   - Hot-plugged: /machine/peripheral/<device-id>/virtio-backend
//     device field is empty; ID is extracted from the QDev path
func buildDeviceMap(devices []qmp.BlockDevice) map[string]string {
	type deviceEntry struct {
		name     string
		pciIndex int
	}

	var virtioDevices []deviceEntry

	for _, dev := range devices {
		if dev.QDev == "" || !strings.Contains(dev.QDev, "virtio-backend") {
			continue
		}

		name := dev.Device
		pciIndex := extractPCIIndex(dev.QDev)

		if pciIndex < 0 {
			// Hot-plugged device: extract name and port from QDev path.
			// QDev looks like "/machine/peripheral/<name>/virtio-backend"
			if name == "" {
				name = extractPeripheralName(dev.QDev)
			}
			if name == "" {
				slog.Warn("Could not extract device name from QDev path", "qdev", dev.QDev)
				continue
			}
			// Use hotplug port number for ordering; fall back to a high
			// index so hot-plugged devices sort after boot devices.
			pciIndex = extractHotplugPort(dev.QDev)
			if pciIndex < 0 {
				pciIndex = 1000 // arbitrary high value
			}
		}

		virtioDevices = append(virtioDevices, deviceEntry{
			name:     name,
			pciIndex: pciIndex,
		})
	}

	// Sort by PCI index — this determines /dev/vd* letter assignment
	sort.Slice(virtioDevices, func(i, j int) bool {
		return virtioDevices[i].pciIndex < virtioDevices[j].pciIndex
	})

	result := make(map[string]string, len(virtioDevices))
	for i, entry := range virtioDevices {
		if i >= 26 {
			slog.Warn("buildDeviceMap: more than 26 virtio devices, cannot map remaining",
				"totalDevices", len(virtioDevices))
			break
		}
		guestDev := fmt.Sprintf("/dev/vd%c", 'a'+i)
		result[entry.name] = guestDev
	}

	return result
}

// UpdateGuestDeviceNames queries the running VM's QMP to discover actual
// guest device paths and updates the instance's BlockDeviceMappings
// accordingly. Persists running state on success. No-op when QMPClient is
// nil. Errors are logged; callers do not need to react.
func (m *Manager) UpdateGuestDeviceNames(instance *VM) {
	if instance.QMPClient == nil {
		slog.Warn("UpdateGuestDeviceNames: QMPClient is nil, cannot discover guest device names",
			"instanceId", instance.ID)
		return
	}

	deviceMap, err := queryGuestDeviceMap(instance.QMPClient, instance.ID)
	if err != nil {
		slog.Warn("Failed to query guest device map, BlockDeviceMappings will use API names",
			"instanceId", instance.ID, "err", err)
		return
	}

	// Build volume ID → guest device path mapping from EBSRequests.
	// Collect under EBSRequests lock, then release before acquiring the
	// manager lock to maintain consistent lock ordering.
	instance.EBSRequests.Mu.Lock()
	volToGuest := make(map[string]string, len(instance.EBSRequests.Requests))
	for _, req := range instance.EBSRequests.Requests {
		var qemuID string
		if req.Boot {
			qemuID = "os"
		} else if req.CloudInit {
			qemuID = "cloudinit"
		} else {
			qemuID = fmt.Sprintf("vdisk-%s", req.Name)
		}
		if gd, ok := deviceMap[qemuID]; ok {
			volToGuest[req.Name] = gd
		}
	}
	instance.EBSRequests.Mu.Unlock()

	m.UpdateState(instance.ID, func(v *VM) {
		if v.Instance == nil {
			return
		}
		for _, bdm := range v.Instance.BlockDeviceMappings {
			if bdm.Ebs == nil || bdm.Ebs.VolumeId == nil || bdm.DeviceName == nil {
				continue
			}
			if gd, ok := volToGuest[*bdm.Ebs.VolumeId]; ok {
				bdm.DeviceName = &gd
			}
		}
	})

	if err := m.writeRunningState(); err != nil {
		slog.Error("Failed to persist state after guest device name update",
			"instanceId", instance.ID, "err", err)
	}

	slog.Info("Updated guest device names", "instanceId", instance.ID, "deviceMap", deviceMap)
}

// extractPCIIndex parses the device index from a QDev path.
// For example: "/machine/peripheral-anon/device[3]/virtio-backend" → 3
func extractPCIIndex(qdev string) int {
	matches := pciAddrRegexp.FindStringSubmatch(qdev)
	if len(matches) < 2 {
		return -1
	}
	idx, err := strconv.Atoi(matches[1])
	if err != nil {
		return -1
	}
	return idx
}

// extractPeripheralName extracts the device ID from a hot-plugged QDev path.
// For example: "/machine/peripheral/vdisk-vol-abc123/virtio-backend" → "vdisk-vol-abc123"
func extractPeripheralName(qdev string) string {
	const prefix = "/machine/peripheral/"
	if !strings.HasPrefix(qdev, prefix) {
		return ""
	}
	rest := qdev[len(prefix):]
	if idx := strings.Index(rest, "/"); idx > 0 {
		return rest[:idx]
	}
	return ""
}

// extractHotplugPort extracts the EBS hot-plug port number from a QDev path.
// For example: "/machine/peripheral/vdisk-vol-xxx/hotplug-ebs3/virtio-backend" → 3
// Returns -1 if no EBS hot-plug port is found.
func extractHotplugPort(qdev string) int {
	matches := hotplugPortRegexp.FindStringSubmatch(qdev)
	if len(matches) < 2 {
		return -1
	}
	idx, err := strconv.Atoi(matches[1])
	if err != nil {
		return -1
	}
	return idx
}

// nextAvailableDevice finds the next available /dev/sd[f-p] device name
// for instance. AWS convention reserves f-p for attached volumes. Returns
// an empty string when every slot is occupied.
func nextAvailableDevice(instance *VM) string {
	usedDevices := make(map[string]bool)

	if instance.Instance != nil {
		for _, bdm := range instance.Instance.BlockDeviceMappings {
			if bdm.DeviceName != nil {
				usedDevices[*bdm.DeviceName] = true
			}
		}
	}

	instance.EBSRequests.Mu.Lock()
	for _, req := range instance.EBSRequests.Requests {
		if req.DeviceName != "" {
			usedDevices[req.DeviceName] = true
		}
	}
	instance.EBSRequests.Mu.Unlock()

	for c := 'f'; c <= 'p'; c++ {
		dev := fmt.Sprintf("/dev/sd%c", c)
		if !usedDevices[dev] {
			return dev
		}
	}

	return ""
}
