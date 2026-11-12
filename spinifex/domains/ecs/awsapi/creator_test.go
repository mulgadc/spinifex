package awsapi

import (
	"errors"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/stretchr/testify/assert"
)

// A service's createdBy names the role behind the session, a task
// definition's registeredBy the session itself; no other action records one.
func TestCreatorARN_RoleForServiceSessionForTaskDefinition(t *testing.T) {
	const role = "arn:aws:iam::123456789012:role/ops/deployer"
	const session = "arn:aws:sts::123456789012:assumed-role/deployer/alice"
	actor := func() (dispatch.Actor, error) {
		return dispatch.Actor{PrincipalType: dispatch.PrincipalTypeAssumedRole, PrincipalARN: role, CallerARN: session}, nil
	}
	assert.Equal(t, role, creatorARN(t.Context(), "CreateService", actor))
	assert.Equal(t, session, creatorARN(t.Context(), "RegisterTaskDefinition", actor))
	assert.Empty(t, creatorARN(t.Context(), "RunTask", actor))
	assert.Empty(t, creatorARN(t.Context(), "CreateService", nil))
	// An unresolved session still names its role, but leaves registeredBy empty.
	noSession := func() (dispatch.Actor, error) { return dispatch.Actor{PrincipalARN: role}, errors.New("no caller ARN") }
	assert.Equal(t, role, creatorARN(t.Context(), "CreateService", noSession))
	assert.Empty(t, creatorARN(t.Context(), "RegisterTaskDefinition", noSession))
}
