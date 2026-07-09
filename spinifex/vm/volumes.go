package vm

import (
	"context"
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

// blockdevDelMaxAttempts caps the bounded retry on "node is in use" as a
// safety net after the DEVICE_DELETED wait (see DeviceDeletedTimeout): a miss
// on the event (timeout, or a client not opted in) falls back to polling at
// DetachDelay. 20 × DetachDelay (default 1s) gives a 20s budget.
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

// delIothreadBestEffort removes the per-volume iothread object on a failed
// attach. Without it the orphaned iothread makes object-add fail on every
// retry ("duplicate property"), so the attach can never recover.
func (m *Manager) delIothreadBestEffort(ctx context.Context, instance *VM, iothreadID, volumeID string) {
	if _, err := sendQMPCommand(ctx, instance.QMPClient, qmp.QMPCommand{
		Execute:   "object-del",
		Arguments: map[string]any{"id": iothreadID},
	}, instance.ID); err != nil {
		slog.WarnContext(ctx, "AttachVolume: rollback object-del iothread failed", "volumeId", volumeID, "err", err)
	}
}

// AttachVolume hot-plugs a volume via the QMP pipeline (mount → blockdev-add →
// device_add). Partial state is rolled back on failure. If device is empty, the
// next free /dev/sd[f-p] slot is allocated. Instance must be in StateRunning.
func (m *Manager) AttachVolume(ctx context.Context, id, volumeID, device string) (AttachVolumeResult, error) {
	instance, ok := m.Get(id)
	if !ok {
		return AttachVolumeResult{}, ErrInstanceNotFound
	}

	if status := m.Status(instance); status != StateRunning {
		return AttachVolumeResult{}, fmt.Errorf("%w: cannot attach to instance %s in state %s",
			ErrInvalidTransition, id, status)
	}

	// Serialize hot-plug per instance so the port allocation below and the
	// matching device_add are atomic; concurrent attaches would otherwise pick
	// the same PCIe root port and the second device_add fails ("slot 0 already
	// occupied"). Detach takes the same lock.
	instance.attachMu.Lock()
	defer instance.attachMu.Unlock()

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
		slog.ErrorContext(ctx, "AttachVolume: ebs.mount failed", "volumeId", volumeID, "err", err)
		// Empty-URI response leaves backend NBD state ambiguous; unmount
		// defensively to avoid orphaning a half-started mount.
		if errors.Is(err, ErrMountAmbiguous) {
			m.rollbackUnmount(ebsRequest)
		}
		return AttachVolumeResult{}, fmt.Errorf("mount volume %s: %w", volumeID, err)
	}

	serverType, socketPath, nbdHost, nbdPort, err := utils.ParseNBDURI(ebsRequest.NBDURI)
	if err != nil {
		slog.ErrorContext(ctx, "AttachVolume: failed to parse NBDURI", "uri", ebsRequest.NBDURI, "err", err)
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

	// Allocate the PCIe hot-plug port from in-memory accounting. QEMU reports
	// block devices by id (/machine/peripheral/<id>/virtio-backend), not by bus,
	// so a live query-block scan cannot tell which hotplug-ebs port is occupied
	// and always returned the first one — every attach past the first collided
	// on slot 0. The recorded HotplugPort per attached volume is authoritative;
	// attachMu (held above) serializes allocation. virtio-blk-pci cannot land on
	// pcie.0 (no hot-plug), so a free hotplug-ebs root port is always required.
	instance.EBSRequests.Mu.Lock()
	hotplugPort := freeHotplugEBSPort(instance.EBSRequests.Requests)
	instance.EBSRequests.Mu.Unlock()
	if hotplugPort == 0 {
		slog.ErrorContext(ctx, "AttachVolume: EBS hot-plug port pool exhausted", "volumeId", volumeID)
		m.rollbackUnmount(ebsRequest)
		return AttachVolumeResult{}, ErrAttachmentLimitExceeded
	}
	ebsRequest.HotplugPort = hotplugPort

	if _, err := sendQMPCommand(ctx, instance.QMPClient, qmp.QMPCommand{
		Execute: "object-add",
		Arguments: map[string]any{
			"qom-type": "iothread",
			"id":       iothreadID,
		},
	}, instance.ID); err != nil {
		slog.ErrorContext(ctx, "AttachVolume: QMP object-add iothread failed", "volumeId", volumeID, "err", err)
		m.rollbackUnmount(ebsRequest)
		return AttachVolumeResult{}, fmt.Errorf("QMP object-add iothread: %w", err)
	}

	if _, err := sendQMPCommand(ctx, instance.QMPClient, qmp.QMPCommand{
		Execute: "blockdev-add",
		Arguments: map[string]any{
			"node-name": nodeName,
			"driver":    "nbd",
			"server":    serverArg,
			"export":    "",
			"read-only": false,
		},
	}, instance.ID); err != nil {
		slog.ErrorContext(ctx, "AttachVolume: QMP blockdev-add failed", "volumeId", volumeID, "err", err)
		m.delIothreadBestEffort(ctx, instance, iothreadID, volumeID)
		m.rollbackUnmount(ebsRequest)
		return AttachVolumeResult{}, fmt.Errorf("QMP blockdev-add: %w", err)
	}

	// The virtio-blk-pci device must land on a free hot-plug PCIe root port
	// (hotplug-ebs{N}); pcie.0 rejects hot-plug. The port is allocated from
	// live QEMU state above, independent of the AWS device name.
	hotplugBus := fmt.Sprintf("hotplug-ebs%d", hotplugPort)

	// serial is the volume-id with dashes stripped ("vol" + 17 hex = 20 bytes,
	// the virtio-blk serial limit). It surfaces in-guest as the block device
	// serial so the EBS CSI node plugin can locate /dev/disk/by-id and match
	// `lsblk -o SERIAL` against the volume-id.
	deviceAddArgs := map[string]any{
		"driver":   "virtio-blk-pci",
		"id":       deviceID,
		"drive":    nodeName,
		"iothread": iothreadID,
		"serial":   strings.ReplaceAll(volumeID, "-", ""),
		"bus":      hotplugBus,
	}

	if _, err := sendQMPCommand(ctx, instance.QMPClient, qmp.QMPCommand{
		Execute:   "device_add",
		Arguments: deviceAddArgs,
	}, instance.ID); err != nil {
		slog.ErrorContext(ctx, "AttachVolume: QMP device_add failed, rolling back blockdev",
			"volumeId", volumeID, "err", err)
		if _, delErr := sendQMPCommand(ctx, instance.QMPClient, qmp.QMPCommand{
			Execute:   "blockdev-del",
			Arguments: map[string]any{"node-name": nodeName},
		}, instance.ID); delErr != nil {
			slog.ErrorContext(ctx, "AttachVolume: rollback blockdev-del failed, skipping EBS unmount",
				"volumeId", volumeID, "err", delErr)
		} else {
			m.delIothreadBestEffort(ctx, instance, iothreadID, volumeID)
			m.rollbackUnmount(ebsRequest)
		}
		return AttachVolumeResult{}, fmt.Errorf("QMP device_add: %w", err)
	}

	// Discover guest device. query-block may not include the device
	// immediately after device_add; queryGuestDeviceMapWait retries.
	guestDevice := device // fallback to AWS API name
	deviceMap, qmpErr := queryGuestDeviceMapWait(ctx, instance.QMPClient, instance.ID, deviceID)
	if qmpErr != nil {
		slog.WarnContext(ctx, "AttachVolume: failed to query guest device map, using API device name",
			"volumeId", volumeID, "err", qmpErr)
	} else if gd, ok := deviceMap[deviceID]; ok {
		guestDevice = gd
		slog.InfoContext(ctx, "AttachVolume: discovered guest device",
			"volumeId", volumeID, "qemuDevice", deviceID, "guestDevice", guestDevice)
	} else {
		slog.ErrorContext(ctx, "AttachVolume: device not found in QMP device map after retries, using API device name",
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
			slog.ErrorContext(ctx, "AttachVolume: failed to update volume metadata",
				"volumeId", volumeID, "err", err)
		}
	}

	if err := m.writeRunningState(); err != nil {
		slog.ErrorContext(ctx, "AttachVolume: failed to write state", "err", err)
	}

	slog.InfoContext(ctx, "Volume attached successfully",
		"volumeId", volumeID, "instanceId", instance.ID,
		"apiDevice", device, "guestDevice", guestDevice)

	return AttachVolumeResult{DeviceName: device, GuestDevice: guestDevice}, nil
}

// DetachVolume hot-unplugs a volume via the QMP pipeline (device_del →
// blockdev-del → object-del → ebs.unmount). Returns the recorded AWS-API device
// name. force=true tolerates device_del failure; blockdev-del is always required.
func (m *Manager) DetachVolume(ctx context.Context, id, volumeID, device string, force bool) (string, error) {
	instance, ok := m.Get(id)
	if !ok {
		return "", ErrInstanceNotFound
	}

	if status := m.Status(instance); status != StateRunning {
		return "", fmt.Errorf("%w: cannot detach from instance %s in state %s",
			ErrInvalidTransition, id, status)
	}

	// Serialize against concurrent attach/detach on this instance's PCIe ports.
	instance.attachMu.Lock()
	defer instance.attachMu.Unlock()

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

	if ebsReq.Boot || ebsReq.EFI {
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
	deviceDelIssued := false
	_, err := sendQMPCommand(ctx, instance.QMPClient, qmp.QMPCommand{
		Execute:   "device_del",
		Arguments: map[string]any{"id": deviceID},
	}, instance.ID)
	switch {
	case err == nil:
		deviceDelIssued = true
	case isQMPDeviceNotFound(err):
		slog.InfoContext(ctx, "DetachVolume: guest device already removed (resuming detach)",
			"volumeId", volumeID, "err", err)
	case force:
		slog.WarnContext(ctx, "DetachVolume: QMP device_del failed (force=true, continuing)",
			"volumeId", volumeID, "err", err)
	default:
		slog.ErrorContext(ctx, "DetachVolume: QMP device_del failed", "volumeId", volumeID, "err", err)
		return "", fmt.Errorf("QMP device_del: %w", err)
	}

	// device_del only requests the unplug; QEMU frees the block node once the
	// guest ACKs it (DEVICE_DELETED). Wait for that real completion signal
	// instead of blindly sleeping, so blockdev-del isn't attempted while the
	// node still has a user. A miss (timeout, event drained by a racing read,
	// or a resumed/forced detach with no fresh unplug in flight) falls
	// through to the bounded retry below unchanged.
	if deviceDelIssued && m.deps.DeviceDeletedTimeout > 0 {
		if waitErr := waitForDeviceDeletedEvent(ctx, instance.QMPClient, deviceID, m.deps.DeviceDeletedTimeout, instance.ID); waitErr != nil {
			slog.DebugContext(ctx, "DetachVolume: DEVICE_DELETED not observed, falling back to blockdev-del retry",
				"volumeId", volumeID, "err", waitErr)
		}
	} else if m.deps.DetachDelay > 0 {
		time.Sleep(m.deps.DetachDelay)
	}

	// blockdev-del with bounded retry on "node is in use".
	if blockdevErr := m.tryBlockdevDel(ctx, instance, nodeName); blockdevErr != nil {
		slog.ErrorContext(ctx, "DetachVolume: QMP blockdev-del failed, leaving volume state intact",
			"volumeId", volumeID, "err", blockdevErr)
		return "", fmt.Errorf("QMP blockdev-del: %w", blockdevErr)
	}

	// object-del (best-effort).
	if _, iothreadErr := sendQMPCommand(ctx, instance.QMPClient, qmp.QMPCommand{
		Execute:   "object-del",
		Arguments: map[string]any{"id": iothreadID},
	}, instance.ID); iothreadErr != nil {
		slog.WarnContext(ctx, "DetachVolume: QMP object-del iothread failed (non-fatal)",
			"volumeId", volumeID, "err", iothreadErr)
	}

	// ebs.unmount drives the synchronous block-map seal to predastore. On
	// failure the volume's local WAL is retained, so keep the volume attached
	// and return the error; an AWS-CLI retry re-drives the seal and same-node
	// reattach still works meanwhile.
	if m.deps.VolumeMounter != nil {
		if err := m.deps.VolumeMounter.UnmountOne(ebsReq); err != nil {
			slog.ErrorContext(ctx, "DetachVolume: ebs.unmount seal failed, leaving volume attached",
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
			slog.ErrorContext(ctx, "DetachVolume: failed to update volume metadata",
				"volumeId", volumeID, "err", err)
		}
	}

	if err := m.writeRunningState(); err != nil {
		slog.ErrorContext(ctx, "DetachVolume: failed to write state", "err", err)
	}

	slog.InfoContext(ctx, "Volume detached successfully", "volumeId", volumeID, "instanceId", instance.ID)
	return ebsReq.DeviceName, nil
}

// waitForDeviceDeletedEvent blocks until QEMU emits a DEVICE_DELETED event
// matching deviceID or timeout elapses, returning nil only on a match. It
// takes over the QMP connection directly (no in-flight command is expected
// while it runs) rather than going through sendQMPCommand, since the signal
// we need is an async event, not a command reply.
//
// A timeout is treated the same as any other decode failure: q.Decoder is a
// single shared *json.Decoder, and once Decode returns an error — including a
// read-deadline timeout — that Decoder instance is permanently unusable, so
// q.Dead is set exactly as sendQMPCommand does. The next QMP command
// transparently redials via reconnectQMP. This is non-fatal either way:
// DetachVolume falls back to the bounded blockdev-del retry, so a missed
// event never blocks the caller forever.
func waitForDeviceDeletedEvent(ctx context.Context, q *qmp.QMPClient, deviceID string, timeout time.Duration, instanceID string) error {
	if q == nil || q.Conn == nil || q.Decoder == nil {
		return fmt.Errorf("QMP client is not initialized")
	}

	q.Mu.Lock()
	defer q.Mu.Unlock()

	if q.Dead {
		return fmt.Errorf("QMP client is disconnected")
	}

	if err := q.Conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("set read deadline: %w", err)
	}
	defer func() { _ = q.Conn.SetReadDeadline(time.Time{}) }()

	for {
		var msg struct {
			Event string `json:"event"`
			Data  struct {
				Device string `json:"device"`
				Path   string `json:"path"`
			} `json:"data"`
		}
		if err := q.Decoder.Decode(&msg); err != nil {
			q.Dead = true
			return fmt.Errorf("decode error waiting for DEVICE_DELETED: %w", err)
		}
		if msg.Event != "DEVICE_DELETED" {
			// Nothing else should be in flight on this connection while we
			// hold q.Mu, but tolerate and skip any stray message rather than
			// treating it as our signal.
			continue
		}
		if msg.Data.Device == deviceID || strings.Contains(msg.Data.Path, deviceID) {
			return nil
		}
		slog.DebugContext(ctx, "waitForDeviceDeletedEvent: DEVICE_DELETED for a different device, still waiting",
			"wantDevice", deviceID, "gotDevice", msg.Data.Device, "instanceId", instanceID)
	}
}

// tryBlockdevDel issues blockdev-del with bounded retry on "is in use" errors.
// QEMU surfaces this as GenericError while NBD teardown and in-flight I/O drain.
func (m *Manager) tryBlockdevDel(ctx context.Context, instance *VM, nodeName string) error {
	var lastErr error
	for attempt := 1; attempt <= blockdevDelMaxAttempts; attempt++ {
		_, err := sendQMPCommand(ctx, instance.QMPClient, qmp.QMPCommand{
			Execute:   "blockdev-del",
			Arguments: map[string]any{"node-name": nodeName},
		}, instance.ID)
		if err == nil {
			if attempt > 1 {
				slog.InfoContext(ctx, "DetachVolume: blockdev-del succeeded after retry",
					"nodeName", nodeName, "attempts", attempt)
			}
			return nil
		}
		// A prior detach already removed the block node (e.g. an AWS-CLI retry
		// after the ebs.unmount seal failed): treat as success so the retry
		// resumes through object-del to the seal instead of wedging on a
		// now-absent node.
		if isQMPNodeNotFound(err) {
			slog.InfoContext(ctx, "DetachVolume: block node already removed (resuming detach)",
				"nodeName", nodeName, "err", err)
			return nil
		}
		lastErr = err
		if !isQMPNodeInUse(err) {
			return err
		}
		if attempt == blockdevDelMaxAttempts {
			break
		}
		slog.DebugContext(ctx, "DetachVolume: blockdev-del busy, retrying",
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

// isQMPNodeNotFound reports whether err is a QMP error for a block node that no
// longer exists. blockdev-del on an already-removed node returns this (QEMU:
// "Failed to find node with node-name=..."), making blockdev-del idempotent
// across detach retries — mirroring isQMPDeviceNotFound for device_del.
func isQMPNodeNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Failed to find node") || strings.Contains(msg, "Cannot find device")
}
