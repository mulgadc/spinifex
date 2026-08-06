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

func TestCheckVersion(t *testing.T) {
	assert.NoError(t, checkVersion(SchemaVersion))
	assert.ErrorIs(t, checkVersion(SchemaVersion+1), ErrUnsupportedVersion)
	assert.ErrorIs(t, checkVersion(0), ErrUnsupportedVersion)
}
