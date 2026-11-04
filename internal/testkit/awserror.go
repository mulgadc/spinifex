package testutil

import (
	"testing"

	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RequireAWSError asserts err carries the AWS code and exact client-facing
// message; an empty message asserts the bare code with no message attached.
func RequireAWSError(t *testing.T, err error, code, message string) {
	t.Helper()
	require.Error(t, err)
	got, msg, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok, "error %v carries no AWS code", err)
	assert.Equal(t, code, got)
	assert.Equal(t, message, msg)
}

// RequireAWSCode asserts err carries the AWS code, whatever its message.
func RequireAWSCode(t *testing.T, err error, code string) {
	t.Helper()
	require.Error(t, err)
	got, ok := awserrors.ResolveErrorCode(err)
	require.True(t, ok, "error %v carries no AWS code", err)
	assert.Equal(t, code, got)
}
