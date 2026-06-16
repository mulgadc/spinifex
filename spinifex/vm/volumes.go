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

// AttachVolumeResult carries the AWS-API device name (/dev/sd[f-p]) and the
// in-guest path (/dev/vd*) discovered via QMP. DeviceName is echoed in the
// API response and volume metadata; GuestDevice is used for diagnostics.
type AttachVolumeResult struct {
	DeviceName  string
	GuestDevice string
}

// rollbackUnmount best-effort unmounts a volume while unwinding a failed
// AttachVolume. The volume never took guest writes, so the unmount seal is a
// no-op; a failure is logged and tolerated rather than masking the attach error.
func (m *Manager) rollbackUnmount(req types.EBSRequest) {
	if m.deps.VolumeMounter == nil {
		return
	}
	if err := m.deps.VolumeMounter.UnmountOne(req); err != nil {
		slog.Warn("AttachVolume: rollback unmount failed", "volume", req.Name, "err", err)
	}
}

// AttachVolume hot-plugs a volume via the QMP pipeline (mount → blockdev-add →
// device_add). Partial state is rolled back on failure. If device is empty, the
// next free /dev/sd[f-p] slot is allocated. Instance must be in StateRunning.
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
			m.rollbackUnmount(ebsRequest)
		}
		return AttachVolumeResult{}, fmt.Errorf("mount volume %s: %w", volumeID, err)
	}

	serverType, socketPath, nbdHost, nbdPort, err := utils.ParseNBDURI(ebsRequest.NBDURI)
	if err != nil {
		slog.Error("AttachVolume: failed to parse NBDURI", "uri", ebsRequest.NBDURI, "err", err)
		m.rollbackUnmount(ebsRequest)
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
		m.rollbackUnmount(ebsRequest)
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
		m.rollbackUnmount(ebsRequest)
		return AttachVolumeResult{}, fmt.Errorf("QMP blockdev-add: %w", err)
	}

	// /dev/sdf -> hotplug-ebs1, /dev/sdg -> hotplug-ebs2, etc. The id prefix
	// matches the pcie-root-port pre-allocation in buildBaseVMConfig; the
	// "-ebs" suffix disambiguates these ports from hotplug-eni{N} (Sprint 3a
	// ENI hot-plug pool).
	hotplugBus := ""
	if len(device) > 0 {
		letter := device[len(device)-1]
		if letter >= 'f' && letter <= 'p' {
			hotplugBus = fmt.Sprintf("hotplug-ebs%d", letter-'f'+1)
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
			m.rollbackUnmount(ebsRequest)
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

	// BlockDeviceMappings[].DeviceName carries the in-guest path so
	// callers who SSH into the VM and `lsblk` see names that match
	// DescribeInstances (mulga-599 / PR #55). UpdateGuestDeviceNames
	// re-applies this convention on every Launch/Start, so any "fix"
	// to use the API name here would be silently overwritten on the
	// next start.
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

	// Volume metadata, by contrast, drives Volume.Attachments[].Device
	// and the attachment.device filter on DescribeVolumes. The Terraform
	// AWS provider polls that filter with the API-form name (/dev/sd[f-p])
	// supplied in the .tf config, so storing the guest path here makes
	// the filter reject every attached volume and the post-attach wait
	// loop fails with "couldn't find resource". Always persist the API
	// name even though it diverges from the BDM convention above —
	// nothing in mulga-599 rewrites this field after attach.
	if m.deps.VolumeStateUpdater != nil {
		if err := m.deps.VolumeStateUpdater.UpdateVolumeState(volumeID, "in-use", instance.ID, device); err != nil {
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

// DetachVolume hot-unplugs a volume via the QMP pipeline (device_del →
// blockdev-del → object-del → ebs.unmount). Returns the recorded AWS-API device
// name. force=true tolerates device_del failure; blockdev-del is always required.
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

	// ebs.unmount drives the synchronous block-map seal to predastore. On
	// failure the volume's local WAL is retained, so keep the volume attached
	// and return the error; an AWS-CLI retry re-drives the seal and same-node
	// reattach still works meanwhile.
	if m.deps.VolumeMounter != nil {
		if err := m.deps.VolumeMounter.UnmountOne(ebsReq); err != nil {
			slog.Error("DetachVolume: ebs.unmount seal failed, leaving volume attached",
				"volumeId", volumeID, "err", err)
			return "", fmt.Errorf("ebs.unmount seal: %w", err)
		}
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

// tryBlockdevDel issues blockdev-del with bounded retry on "is in use" errors.
// QEMU surfaces this as GenericError while NBD teardown and in-flight I/O drain.
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

// isQMPDeviceNotFound reports whether err is a QMP DeviceNotFound error,
// making device_del idempotent across retries.
func isQMPDeviceNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "DeviceNotFound")
}

// isQMPNodeInUse reports whether err is a QMP GenericError for a node still in
// use. Matches both "is in use" and "is still in use" phrasing.
func isQMPNodeInUse(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "in use")
}
