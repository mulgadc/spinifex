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
)

type ErrorCode string

const (
	ErrorCodeAlreadyExists      ErrorCode = "already_exists"
	ErrorCodeInvalidArgument    ErrorCode = "invalid_argument"
	ErrorCodeNotFound           ErrorCode = "not_found"
	ErrorCodeUnsupportedVersion ErrorCode = "unsupported_version"
	ErrorCodeVolumeInUse        ErrorCode = "volume_in_use"
	ErrorCodeUnsupportedCap     ErrorCode = "unsupported_capability"
	ErrorCodeInternal           ErrorCode = "internal"
)

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
