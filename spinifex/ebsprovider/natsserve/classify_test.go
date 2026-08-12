package natsserve

import (
	"fmt"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/stretchr/testify/assert"
)

// TestClassifyError pins every sentinel to the code it crosses the wire as. A
// sentinel missing from classifyError falls through to internal, which tells a
// caller a retryable refusal is permanent and loses the reason with it.
func TestClassifyError(t *testing.T) {
	tests := []struct {
		err  error
		want ebsprovider.ErrorCode
	}{
		{ebsprovider.ErrAlreadyExists, ebsprovider.ErrorCodeAlreadyExists},
		{ebsprovider.ErrInvalidArgument, ebsprovider.ErrorCodeInvalidArgument},
		{ebsprovider.ErrNotFound, ebsprovider.ErrorCodeNotFound},
		{ebsprovider.ErrUnsupportedVersion, ebsprovider.ErrorCodeUnsupportedVersion},
		{ebsprovider.ErrVolumeInUse, ebsprovider.ErrorCodeVolumeInUse},
		{ebsprovider.ErrUnsupportedCapability, ebsprovider.ErrorCodeUnsupportedCap},
		{ebsprovider.ErrUnavailable, ebsprovider.ErrorCodeUnavailable},
	}

	for _, tt := range tests {
		t.Run(string(tt.want), func(t *testing.T) {
			wrapped := fmt.Errorf("vol-1: %w", tt.err)
			got := classifyError(wrapped)
			assert.Equal(t, tt.want, got.Code)
			assert.Equal(t, wrapped.Error(), got.Message, "the wrapping context must survive classification")
		})
	}

	t.Run("unrecognised error is internal", func(t *testing.T) {
		got := classifyError(fmt.Errorf("something unexpected"))
		assert.Equal(t, ebsprovider.ErrorCodeInternal, got.Code)
		assert.False(t, got.Code.Retryable(), "an error we cannot classify must not invite a retry loop")
	})
}
