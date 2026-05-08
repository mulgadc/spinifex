package vm

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/qmp"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// blockdevDelMaxAttempts caps the bounded retry on "node is in use" while
// QEMU drains pending I/O after device_del. 20 × DetachDelay (default 1s)
// gives a 20s budget.
const blockdevDelMaxAttempts = 20

// AttachVolumeResult carries the device names produced by AttachVolume:
// the AWS-API name (/dev/sdf, possibly auto-allocated) and the discovered
// guest device path (/dev/vdb…). Daemon handlers use GuestDevice in the
// AWS API response; the discovered name eventually appears in
// DescribeInstances.
type AttachVolumeResult struct {
	DeviceName  string
	GuestDevice string
}

// AttachVolume hot-plugs a volume into a running instance via the
// three-phase QMP pipeline (mount → blockdev-add → device_add). On
// failure mid-pipeline, partial state is rolled back. The instance must
// be in StateRunning. If device is empty, the next free /dev/sd[f-p]
// slot is allocated.
//
// Daemon handlers retain volume-side validation (existence, ownership,
// availability, AZ) and emit the AWS API response; the manager owns
// every QMP and persistence side-effect.
func (m *Manager) AttachVolume(id, volumeID, device string) (AttachVolumeResult, error) {
	instance, ok := m.Get(id)
	if !ok {
		return AttachVolumeResult{}, ErrInstanceNotFound
	}

	if status := m.Status(instance); status != StateRunning {
		return AttachVolumeResult{}, fmt.Errorf("%w: cannot attach to instance %s in state %s",
			ErrInvalidTransition, id, status)
	}

	if device == "" {
		m.UpdateState(id, func(v *VM) { device = nextAvailableDevice(v) })
		if device == "" {
			return AttachVolumeResult{}, ErrAttachmentLimitExceeded
		}
	}

	ebsRequest := types.EBSRequest{
		Name:       volumeID,
		DeviceName: device,
	}

	if m.deps.VolumeMounter == nil {
		return AttachVolumeResult{}, fmt.Errorf("VolumeMounter not wired")
	}
	if err := m.deps.VolumeMounter.MountOne(&ebsRequest); err != nil {
		slog.Error("AttachVolume: ebs.mount failed", "volumeId", volumeID, "err", err)
		// Empty-URI response leaves backend NBD state ambiguous; unmount
		// defensively to avoid orphaning a half-started mount.
		if errors.Is(err, ErrMountAmbiguous) {
			m.deps.VolumeMounter.UnmountOne(ebsRequest)
		}
		return AttachVolumeResult{}, fmt.Errorf("mount volume %s: %w", volumeID, err)
	}

	serverType, socketPath, nbdHost, nbdPort, err := utils.ParseNBDURI(ebsRequest.NBDURI)
	if err != nil {
		slog.Error("AttachVolume: failed to parse NBDURI", "uri", ebsRequest.NBDURI, "err", err)
		m.deps.VolumeMounter.UnmountOne(ebsRequest)
		return AttachVolumeResult{}, fmt.Errorf("parse NBDURI: %w", err)
	}

	var serverArg map[string]any
	if serverType == "unix" {
		serverArg = map[string]any{"type": "unix", "path": socketPath}
	} else {
		serverArg = map[string]any{"type": "inet", "host": nbdHost, "port": strconv.Itoa(nbdPort)}
	}

	nodeName := fmt.Sprintf("nbd-%s", volumeID)
	deviceID := fmt.Sprintf("vdisk-%s", volumeID)
	iothreadID := fmt.Sprintf("ioth-%s", volumeID)

	if _, err := sendQMPCommand(instance.QMPClient, qmp.QMPCommand{
		Execute: "object-add",
		Arguments: map[string]any{
			"qom-type": "iothread",
			"id":       iothreadID,
		},
	}, instance.ID); err != nil {
		slog.Error("AttachVolume: QMP object-add iothread failed", "volumeId", volumeID, "err", err)
		m.deps.VolumeMounter.UnmountOne(ebsRequest)
		return AttachVolumeResult{}, fmt.Errorf("QMP object-add iothread: %w", err)
	}

	if _, err := sendQMPCommand(instance.QMPClient, qmp.QMPCommand{
		Execute: "blockdev-add",
		Arguments: map[string]any{
			"node-name": nodeName,
			"driver":    "nbd",
			"server":    serverArg,
			"export":    "",
			"read-only": false,
		},
	}, instance.ID); err != nil {
		slog.Error("AttachVolume: QMP blockdev-add failed", "volumeId", volumeID, "err", err)
		m.deps.VolumeMounter.UnmountOne(ebsRequest)
		return AttachVolumeResult{}, fmt.Errorf("QMP blockdev-add: %w", err)
	}

	// /dev/sdf -> hotplug1, /dev/sdg -> hotplug2, etc.
	hotplugBus := ""
	if len(device) > 0 {
		letter := device[len(device)-1]
		if letter >= 'f' && letter <= 'p' {
			hotplugBus = fmt.Sprintf("hotplug%d", letter-'f'+1)
		}
	}

	deviceAddArgs := map[string]any{
		"driver":   "virtio-blk-pci",
		"id":       deviceID,
		"drive":    nodeName,
		"iothread": iothreadID,
	}
	if hotplugBus != "" {
		deviceAddArgs["bus"] = hotplugBus
	}

	if _, err := sendQMPCommand(instance.QMPClient, qmp.QMPCommand{
		Execute:   "device_add",
		Arguments: deviceAddArgs,
	}, instance.ID); err != nil {
		slog.Error("AttachVolume: QMP device_add failed, rolling back blockdev",
			"volumeId", volumeID, "err", err)
		if _, delErr := sendQMPCommand(instance.QMPClient, qmp.QMPCommand{
			Execute:   "blockdev-del",
			Arguments: map[string]any{"node-name": nodeName},
		}, instance.ID); delErr != nil {
			slog.Error("AttachVolume: rollback blockdev-del failed, skipping EBS unmount",
				"volumeId", volumeID, "err", delErr)
		} else {
			m.deps.VolumeMounter.UnmountOne(ebsRequest)
		}
		return AttachVolumeResult{}, fmt.Errorf("QMP device_add: %w", err)
	}

	// Discover guest device. query-block may not include the device
	// immediately after device_add; queryGuestDeviceMapWait retries.
	guestDevice := device // fallback to AWS API name
	deviceMap, qmpErr := queryGuestDeviceMapWait(instance.QMPClient, instance.ID, deviceID)
	if qmpErr != nil {
		slog.Warn("AttachVolume: failed to query guest device map, using API device name",
			"volumeId", volumeID, "err", qmpErr)
	} else if gd, ok := deviceMap[deviceID]; ok {
		guestDevice = gd
		slog.Info("AttachVolume: discovered guest device",
			"volumeId", volumeID, "qemuDevice", deviceID, "guestDevice", guestDevice)
	} else {
		slog.Error("AttachVolume: device not found in QMP device map after retries, using API device name",
			"volumeId", volumeID, "qemuDevice", deviceID, "deviceMap", deviceMap)
	}

	// Replace stale entry for volumeID (covers stop/start cycles that keep
	// the original request) or append a new one.
	instance.EBSRequests.Mu.Lock()
	replaced := false
	for i, req := range instance.EBSRequests.Requests {
		if req.Name == volumeID {
			instance.EBSRequests.Requests[i] = ebsRequest
			replaced = true
			break
		}
	}
	if !replaced {
		instance.EBSRequests.Requests = append(instance.EBSRequests.Requests, ebsRequest)
	}
	instance.EBSRequests.Mu.Unlock()

	m.UpdateState(id, func(v *VM) {
		if v.Instance == nil {
			return
		}
		now := time.Now()
		mapping := &ec2.InstanceBlockDeviceMapping{}
		mapping.SetDeviceName(guestDevice)
		mapping.Ebs = &ec2.EbsInstanceBlockDevice{}
		mapping.Ebs.SetVolumeId(volumeID)
		mapping.Ebs.SetAttachTime(now)
		mapping.Ebs.SetDeleteOnTermination(false)
		mapping.Ebs.SetStatus("attached")
		v.Instance.BlockDeviceMappings = append(v.Instance.BlockDeviceMappings, mapping)
	})

	if m.deps.VolumeStateUpdater != nil {
		if err := m.deps.VolumeStateUpdater.UpdateVolumeState(volumeID, "in-use", instance.ID, guestDevice); err != nil {
			slog.Error("AttachVolume: failed to update volume metadata",
				"volumeId", volumeID, "err", err)
		}
	}

	if err := m.writeRunningState(); err != nil {
		slog.Error("AttachVolume: failed to write state", "err", err)
	}

	slog.Info("Volume attached successfully",
		"volumeId", volumeID, "instanceId", instance.ID,
		"apiDevice", device, "guestDevice", guestDevice)

	return AttachVolumeResult{DeviceName: device, GuestDevice: guestDevice}, nil
}

// DetachVolume hot-unplugs a volume from a running instance via the
// three-phase QMP pipeline (device_del → blockdev-del → object-del →
// ebs.unmount). Returns the recorded AWS-API device name on success so
// the daemon handler can echo it in the response.
//
// device may be empty (no cross-check) or must equal the recorded
// DeviceName. force=true tolerates a failing device_del; blockdev-del
// is always required because tearing down the NBD server underneath a
// live block node would crash the VM.
func (m *Manager) DetachVolume(id, volumeID, device string, force bool) (string, error) {
	instance, ok := m.Get(id)
	if !ok {
		return "", ErrInstanceNotFound
	}

	if status := m.Status(instance); status != StateRunning {
		return "", fmt.Errorf("%w: cannot detach from instance %s in state %s",
			ErrInvalidTransition, id, status)
	}

	instance.EBSRequests.Mu.Lock()
	var ebsReq types.EBSRequest
	found := false
	for _, req := range instance.EBSRequests.Requests {
		if req.Name == volumeID {
			ebsReq = req
			found = true
			break
		}
	}
	instance.EBSRequests.Mu.Unlock()

	if !found {
		return "", fmt.Errorf("%w: %s", ErrVolumeNotAttached, volumeID)
	}

	if ebsReq.Boot || ebsReq.EFI || ebsReq.CloudInit {
		return "", fmt.Errorf("%w: %s", ErrVolumeNotDetachable, volumeID)
	}

	if device != "" && ebsReq.DeviceName != "" && device != ebsReq.DeviceName {
		return "", fmt.Errorf("%w: requested %s, recorded %s",
			ErrVolumeDeviceMismatch, device, ebsReq.DeviceName)
	}

	deviceID := fmt.Sprintf("vdisk-%s", volumeID)
	nodeName := fmt.Sprintf("nbd-%s", volumeID)
	iothreadID := fmt.Sprintf("ioth-%s", volumeID)

	// device_del is idempotent on DeviceNotFound so a second AWS-CLI
	// retry can drive blockdev-del to completion when a prior detach left
	// the guest device gone but the block node intact.
	_, err := sendQMPCommand(instance.QMPClient, qmp.QMPCommand{
		Execute:   "device_del",
		Arguments: map[string]any{"id": deviceID},
	}, instance.ID)
	switch {
	case err == nil:
	case isQMPDeviceNotFound(err):
		slog.Info("DetachVolume: guest device already removed (resuming detach)",
			"volumeId", volumeID, "err", err)
	case force:
		slog.Warn("DetachVolume: QMP device_del failed (force=true, continuing)",
			"volumeId", volumeID, "err", err)
	default:
		slog.Error("DetachVolume: QMP device_del failed", "volumeId", volumeID, "err", err)
		return "", fmt.Errorf("QMP device_del: %w", err)
	}

	if m.deps.DetachDelay > 0 {
		time.Sleep(m.deps.DetachDelay)
	}

	// blockdev-del with bounded retry on "node is in use".
	if blockdevErr := m.tryBlockdevDel(instance, nodeName); blockdevErr != nil {
		slog.Error("DetachVolume: QMP blockdev-del failed, leaving volume state intact",
			"volumeId", volumeID, "err", blockdevErr)
		return "", fmt.Errorf("QMP blockdev-del: %w", blockdevErr)
	}

	// object-del (best-effort).
	if _, iothreadErr := sendQMPCommand(instance.QMPClient, qmp.QMPCommand{
		Execute:   "object-del",
		Arguments: map[string]any{"id": iothreadID},
	}, instance.ID); iothreadErr != nil {
		slog.Warn("DetachVolume: QMP object-del iothread failed (non-fatal)",
			"volumeId", volumeID, "err", iothreadErr)
	}

	// ebs.unmount (best-effort).
	if m.deps.VolumeMounter != nil {
		m.deps.VolumeMounter.UnmountOne(ebsReq)
	}

	// State cleanup.
	instance.EBSRequests.Mu.Lock()
	for i, req := range instance.EBSRequests.Requests {
		if req.Name == volumeID {
			instance.EBSRequests.Requests = append(instance.EBSRequests.Requests[:i], instance.EBSRequests.Requests[i+1:]...)
			break
		}
	}
	instance.EBSRequests.Mu.Unlock()

	m.UpdateState(id, func(v *VM) {
		if v.Instance == nil {
			return
		}
		filtered := make([]*ec2.InstanceBlockDeviceMapping, 0, len(v.Instance.BlockDeviceMappings))
		for _, bdm := range v.Instance.BlockDeviceMappings {
			if bdm.Ebs != nil && bdm.Ebs.VolumeId != nil && *bdm.Ebs.VolumeId == volumeID {
				continue
			}
			filtered = append(filtered, bdm)
		}
		v.Instance.BlockDeviceMappings = filtered
	})

	if m.deps.VolumeStateUpdater != nil {
		if err := m.deps.VolumeStateUpdater.UpdateVolumeState(volumeID, "available", "", ""); err != nil {
			slog.Error("DetachVolume: failed to update volume metadata",
				"volumeId", volumeID, "err", err)
		}
	}

	if err := m.writeRunningState(); err != nil {
		slog.Error("DetachVolume: failed to write state", "err", err)
	}

	slog.Info("Volume detached successfully", "volumeId", volumeID, "instanceId", instance.ID)
	return ebsReq.DeviceName, nil
}

// tryBlockdevDel issues blockdev-del with bounded retry on "is in use"
// errors. After device_del, NBD client teardown and any in-flight guest
// I/O can briefly hold the block node; QEMU surfaces this as a
// GenericError carrying "Node <name> is in use". Polling at DetachDelay
// gives the I/O drain enough time without a hardcoded long sleep.
func (m *Manager) tryBlockdevDel(instance *VM, nodeName string) error {
	var lastErr error
	for attempt := 1; attempt <= blockdevDelMaxAttempts; attempt++ {
		_, err := sendQMPCommand(instance.QMPClient, qmp.QMPCommand{
			Execute:   "blockdev-del",
			Arguments: map[string]any{"node-name": nodeName},
		}, instance.ID)
		if err == nil {
			if attempt > 1 {
				slog.Info("DetachVolume: blockdev-del succeeded after retry",
					"nodeName", nodeName, "attempts", attempt)
			}
			return nil
		}
		lastErr = err
		if !isQMPNodeInUse(err) {
			return err
		}
		if attempt == blockdevDelMaxAttempts {
			break
		}
		slog.Debug("DetachVolume: blockdev-del busy, retrying",
			"nodeName", nodeName, "attempt", attempt, "err", err)
		if m.deps.DetachDelay > 0 {
			time.Sleep(m.deps.DetachDelay)
		}
	}
	return lastErr
}

// isQMPDeviceNotFound returns true when err is a QMP DeviceNotFound class
// error from device_del. Used to make device_del idempotent across detach
// retries.
func isQMPDeviceNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "DeviceNotFound")
}

// isQMPNodeInUse returns true when err is a QMP GenericError reporting a
// block node still in use. blockdev-del fails this way while NBD client
// teardown and queued I/O drain after device_del. QEMU phrases this as
// both "X is in use" and "X is still in use" — match "in use".
func isQMPNodeInUse(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "in use")
}
