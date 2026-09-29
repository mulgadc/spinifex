package vm

import "errors"

// ErrMountRetryable is returned (wrapped) by VolumeMounter.Mount when the
// backing store rejected the mount because it is not yet ready rather than
// permanently unavailable. relaunchAll retries on this sentinel instead of
// failing the instance immediately.
var ErrMountRetryable = errors.New("volume mount failed against a not-yet-ready backing store")
