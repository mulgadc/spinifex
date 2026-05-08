package vm

import "errors"

// ErrInstanceNotFound is returned by manager methods that look up an
// instance by id when no entry exists in the running map.
var ErrInstanceNotFound = errors.New("instance not found")

// ErrInvalidTransition is returned when a lifecycle method cannot run
// because the instance's current state does not permit the target
// transition (e.g. Stop on an already-stopped instance).
var ErrInvalidTransition = errors.New("invalid state transition")

// ErrAttachmentLimitExceeded is returned by AttachVolume when the
// instance has no free /dev/sd[f-p] slot to assign.
var ErrAttachmentLimitExceeded = errors.New("attachment limit exceeded")

// ErrVolumeNotAttached is returned by DetachVolume when the supplied
// volumeID is not present in the instance's EBSRequests.
var ErrVolumeNotAttached = errors.New("volume not attached to instance")

// ErrVolumeNotDetachable is returned by DetachVolume when the target
// volume is a boot, EFI, or cloud-init volume that cannot be hot-unplugged.
var ErrVolumeNotDetachable = errors.New("volume cannot be detached")

// ErrVolumeDeviceMismatch is returned by DetachVolume when the caller
// supplies a device name that does not match the recorded attachment.
var ErrVolumeDeviceMismatch = errors.New("volume device name mismatch")

// ErrMountAmbiguous is returned by VolumeMounter.MountOne when the
// backend acknowledged the mount but returned an empty URI. The mount may
// or may not have started serving NBD; AttachVolume must invoke UnmountOne
// defensively so a half-started backend mount is not orphaned.
var ErrMountAmbiguous = errors.New("ebs.mount succeeded with empty URI")
