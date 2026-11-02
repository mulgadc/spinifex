package natsserve

import (
	"fmt"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
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
		{ebsprovider.ErrVolumeHeldElsewhere, ebsprovider.ErrorCodeVolumeHeldElsewhere},
	}

	for _, tt := range tests {
		t.Run(string(tt.want), func(t *testing.T) {
			wrapped := fmt.Errorf("vol-1: %w", tt.err)
			got := classifyError(ebsprovider.SchemaVersion, wrapped)
			assert.Equal(t, tt.want, got.Code)
			assert.Equal(t, wrapped.Error(), got.Message, "the wrapping context must survive classification")
		})
	}

	t.Run("unrecognised error is internal", func(t *testing.T) {
		got := classifyError(ebsprovider.SchemaVersion, fmt.Errorf("something unexpected"))
		assert.Equal(t, ebsprovider.ErrorCodeInternal, got.Code)
		assert.False(t, got.Code.Retryable(), "an error we cannot classify must not invite a retry loop")
	})

	// A peer below version 3 does not know the code, so it gets the one
	// v1.23.0 sent for the same refusal.
	t.Run("held elsewhere is volume_in_use below version 3", func(t *testing.T) {
		for _, version := range []uint16{1, 2} {
			got := classifyError(version, fmt.Errorf("vol-1: %w", ebsprovider.ErrVolumeHeldElsewhere))
			assert.Equal(t, ebsprovider.ErrorCodeVolumeInUse, got.Code, "version %d", version)
		}
	})
}
