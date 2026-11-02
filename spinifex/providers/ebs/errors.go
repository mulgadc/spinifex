package ebsprovider

import (
	"errors"
	"fmt"
)

var (
	ErrAlreadyExists      = errors.New("resource already exists with different parameters")
	ErrInvalidArgument    = errors.New("invalid argument")
	ErrNotFound           = errors.New("resource not found")
	ErrUnsupportedVersion = errors.New("unsupported EBS provider schema version")
	ErrVolumeInUse        = errors.New("volume is in use")

	// ErrVolumeHeldElsewhere is a publish refused because another node holds
	// the volume's claim. It wraps ErrVolumeInUse, so a caller that only asks
	// whether the volume is in use still matches it.
	ErrVolumeHeldElsewhere = fmt.Errorf("volume is held by another node: %w", ErrVolumeInUse)

	// ErrUnsupportedCapability is returned when a request asks for optional
	// behaviour this provider does not advertise in Capabilities. It is
	// distinct from ErrInvalidArgument: the request is well formed.
	ErrUnsupportedCapability = errors.New("provider does not support the requested capability")

	// ErrUnavailable is returned when the provider could not answer now and
	// the same request may succeed later: a peer it had to consult was
	// unreachable, or state it had to establish could not be established.
	//
	// It is the only code that says anything about retrying. Every other one
	// classifies what went wrong, which leaves a caller unable to tell a
	// permanent refusal from a transient one; returning ErrInternal for a
	// transient condition tells a caller to give up when it should retry.
	ErrUnavailable = errors.New("provider is temporarily unable to answer")
)

// HeldElsewhereSchemaVersion is the first version a held-elsewhere refusal is
// reported as such. Earlier requests get volume_in_use, as v1.23.0 sends it.
const HeldElsewhereSchemaVersion uint16 = 3

// HeldElsewhereError is the sentinel a held-elsewhere refusal of a request at
// version wraps: ErrVolumeHeldElsewhere from HeldElsewhereSchemaVersion on.
func HeldElsewhereError(version uint16) error {
	if version >= HeldElsewhereSchemaVersion {
		return ErrVolumeHeldElsewhere
	}
	return ErrVolumeInUse
}

// ErrorCode is the wire form of a provider error in ProviderError. Unwrap maps
// every code but internal back to the matching Err sentinel.
type ErrorCode string

const (
	ErrorCodeAlreadyExists       ErrorCode = "already_exists"
	ErrorCodeInvalidArgument     ErrorCode = "invalid_argument"
	ErrorCodeNotFound            ErrorCode = "not_found"
	ErrorCodeUnsupportedVersion  ErrorCode = "unsupported_version"
	ErrorCodeVolumeInUse         ErrorCode = "volume_in_use"
	ErrorCodeVolumeHeldElsewhere ErrorCode = "volume_held_elsewhere"
	ErrorCodeUnsupportedCap      ErrorCode = "unsupported_capability"
	// ErrorCodeUnavailable is the retry-later code: among others, a mount
	// against a backing store that is not ready yet returns it.
	ErrorCodeUnavailable ErrorCode = "unavailable"
	ErrorCodeInternal    ErrorCode = "internal"
)

// Retryable reports whether repeating the identical request could succeed.
// Callers branch on this rather than on the code, so adding a transient code
// later does not need every call site to be found again.
func (c ErrorCode) Retryable() bool {
	return c == ErrorCodeUnavailable
}

// ProviderError is the stable error representation carried over NATS.
type ProviderError struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

func (e *ProviderError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (e *ProviderError) Unwrap() error {
	if e == nil {
		return nil
	}
	switch e.Code {
	case ErrorCodeAlreadyExists:
		return ErrAlreadyExists
	case ErrorCodeInvalidArgument:
		return ErrInvalidArgument
	case ErrorCodeNotFound:
		return ErrNotFound
	case ErrorCodeUnsupportedVersion:
		return ErrUnsupportedVersion
	case ErrorCodeVolumeInUse:
		return ErrVolumeInUse
	case ErrorCodeVolumeHeldElsewhere:
		return ErrVolumeHeldElsewhere
	case ErrorCodeUnsupportedCap:
		return ErrUnsupportedCapability
	case ErrorCodeUnavailable:
		return ErrUnavailable
	default:
		return nil
	}
}

func checkVersion(version uint16) error {
	if !SupportsSchemaVersion(version) {
		return fmt.Errorf("%w: got %d, %s", ErrUnsupportedVersion, version, SupportedVersionsText())
	}
	return nil
}

// SupportedVersionsText names the served version range for error messages.
func SupportedVersionsText() string {
	return fmt.Sprintf("supported versions are %d to %d", LegacySchemaVersion, SchemaVersion)
}
