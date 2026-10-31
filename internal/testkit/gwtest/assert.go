package gwtest

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// AssertDenied matches on the resolved code, not the message: a policy denial
// names the principal, action and resource ahead of the code.
func AssertDenied(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	code, ok := awserrors.ResolveErrorCode(err)
	require.True(t, ok, "unresolvable error code: %v", err)
	assert.Equal(t, awserrors.ErrorAccessDenied, code)
}

// AssertUnauthorized asserts EC2's policy denial, UnauthorizedOperation.
func AssertUnauthorized(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorUnauthorizedOperation, err.Error())
}

// AssertPermitted asserts the policy gate passed. The request then fails on the
// absent NATS connection rather than on authorization, so a denial cannot hide
// behind a not-found.
func AssertPermitted(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}
