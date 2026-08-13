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

type ErrorCode string

const (
	ErrorCodeAlreadyExists      ErrorCode = "already_exists"
	ErrorCodeInvalidArgument    ErrorCode = "invalid_argument"
	ErrorCodeNotFound           ErrorCode = "not_found"
	ErrorCodeUnsupportedVersion ErrorCode = "unsupported_version"
	ErrorCodeVolumeInUse        ErrorCode = "volume_in_use"
	ErrorCodeUnsupportedCap     ErrorCode = "unsupported_capability"
	ErrorCodeUnavailable        ErrorCode = "unavailable"
	ErrorCodeInternal           ErrorCode = "internal"
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
	case ErrorCodeUnsupportedCap:
		return ErrUnsupportedCapability
	case ErrorCodeUnavailable:
		return ErrUnavailable
	default:
		return nil
	}
}

func checkVersion(version uint16) error {
	if version != SchemaVersion {
		return fmt.Errorf("%w: got %d, want %d", ErrUnsupportedVersion, version, SchemaVersion)
	}
	return nil
}
