package ebsprovider

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestProviderErrorUnwrapTable covers every ErrorCode's mapping back to its
// sentinel error, plus the two codes with no sentinel (internal, unknown)
// and the nil-receiver methods a caller can hit via a typed-nil *ProviderError.
func TestProviderErrorUnwrapTable(t *testing.T) {
	tests := []struct {
		code ErrorCode
		want error
	}{
		{ErrorCodeAlreadyExists, ErrAlreadyExists},
		{ErrorCodeInvalidArgument, ErrInvalidArgument},
		{ErrorCodeNotFound, ErrNotFound},
		{ErrorCodeUnsupportedVersion, ErrUnsupportedVersion},
		{ErrorCodeVolumeInUse, ErrVolumeInUse},
		{ErrorCodeUnsupportedCap, ErrUnsupportedCapability},
		{ErrorCodeUnavailable, ErrUnavailable},
	}
	for _, tt := range tests {
		t.Run(string(tt.code), func(t *testing.T) {
			err := &ProviderError{Code: tt.code, Message: "msg: " + string(tt.code)}
			assert.ErrorIs(t, err, tt.want)
			assert.Equal(t, "msg: "+string(tt.code), err.Error())
		})
	}

	t.Run("internal code has no sentinel", func(t *testing.T) {
		err := &ProviderError{Code: ErrorCodeInternal, Message: "boom"}
		assert.NoError(t, err.Unwrap())
	})

	t.Run("unknown code has no sentinel", func(t *testing.T) {
		err := &ProviderError{Code: ErrorCode("bogus"), Message: "boom"}
		assert.NoError(t, err.Unwrap())
	})

	t.Run("nil receiver", func(t *testing.T) {
		var err *ProviderError
		assert.NoError(t, err.Unwrap())
		assert.Empty(t, err.Error())
	})
}

// TestErrorCodeRetryable pins which refusals a caller may repeat. Every code
// here is named, so adding one without deciding its retry meaning fails rather
// than defaulting to permanent and stranding whatever the caller was doing.
func TestErrorCodeRetryable(t *testing.T) {
	retryable := []ErrorCode{ErrorCodeUnavailable}
	permanent := []ErrorCode{
		ErrorCodeAlreadyExists,
		ErrorCodeInvalidArgument,
		ErrorCodeNotFound,
		ErrorCodeUnsupportedVersion,
		ErrorCodeVolumeInUse,
		ErrorCodeUnsupportedCap,
		ErrorCodeInternal,
	}

	for _, code := range retryable {
		assert.Truef(t, code.Retryable(), "%s must read as retryable, or a caller gives up on a request that would have succeeded", code)
	}
	for _, code := range permanent {
		assert.Falsef(t, code.Retryable(), "%s must read as permanent, or a caller retries a request that can never succeed", code)
	}
	assert.False(t, ErrorCode("bogus").Retryable(), "an unrecognised code must not invite a retry loop")
}

func TestCheckVersion(t *testing.T) {
	assert.NoError(t, checkVersion(SchemaVersion))
	assert.ErrorIs(t, checkVersion(SchemaVersion+1), ErrUnsupportedVersion)
	assert.ErrorIs(t, checkVersion(0), ErrUnsupportedVersion)
}
