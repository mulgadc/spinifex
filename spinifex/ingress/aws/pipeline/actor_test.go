package pipeline_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActorFor_User(t *testing.T) {
	actor, err := pipeline.ActorFor(credential.Principal{
		AccountID:     "123456789012",
		Identity:      "alice",
		PrincipalType: "user",
		UserID:        "AIDAEXAMPLE",
		CallerARN:     "arn:aws:iam::123456789012:user/alice",
	}, "AKIAEXAMPLE")
	require.NoError(t, err)
	assert.Equal(t, dispatch.Actor{
		AccountID:     "123456789012",
		CallerARN:     "arn:aws:iam::123456789012:user/alice",
		PrincipalType: "user",
		AccessKeyID:   "AKIAEXAMPLE",
		UserID:        "AIDAEXAMPLE",
		PrincipalARN:  "arn:aws:iam::123456789012:user/alice",
		UserName:      "alice",
	}, actor)
}

func TestActorFor_AssumedRoleKeepsSessionAndRole(t *testing.T) {
	actor, err := pipeline.ActorFor(credential.Principal{
		AccountID:         "123456789012",
		Identity:          "session-1",
		PrincipalType:     dispatch.PrincipalTypeAssumedRole,
		UnderlyingRoleARN: "arn:aws:iam::123456789012:role/app",
		CallerARN:         "arn:aws:sts::123456789012:assumed-role/app/session-1",
	}, "ASIAEXAMPLE")
	require.NoError(t, err)
	assert.Equal(t, "session-1", actor.SessionName)
	assert.Empty(t, actor.UserName)
	assert.Equal(t, "arn:aws:iam::123456789012:role/app", actor.PrincipalARN)
	assert.Equal(t, "app", actor.RoleName())
}

func TestActorFor_NoCallerARNReportsErrorButStaysPopulated(t *testing.T) {
	actor, err := pipeline.ActorFor(credential.Principal{
		AccountID:     "123456789012",
		Identity:      "alice",
		PrincipalType: "user",
	}, "AKIAEXAMPLE")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
	assert.Equal(t, "123456789012", actor.AccountID)
	assert.Equal(t, "alice", actor.UserName)
}
