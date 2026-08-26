package handlers_sts

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// roleGetterStub answers GetRole from a name-to-stored-ARN map, the way IAM
// keys roles, and reports NoSuchEntity for anything else.
type roleGetterStub map[string]string

func (s roleGetterStub) GetRole(_ string, input *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	arn, ok := s[aws.StringValue(input.RoleName)]
	if !ok {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return &iam.GetRoleOutput{Role: &iam.Role{RoleName: input.RoleName, Arn: aws.String(arn)}}, nil
}

// TestResolveRoleByARN pins the comparison every caller-supplied role ARN rests
// on. The lookup discards any path the caller wrote, so an ARN that is not the
// one IAM stored must not reach the role its trailing name happens to match.
func TestResolveRoleByARN(t *testing.T) {
	const account = "000000000000"
	roles := roleGetterStub{
		"admin":      "arn:aws:iam::000000000000:role/admin",
		"app-worker": "arn:aws:iam::000000000000:role/team/app-worker",
	}

	t.Run("stored ARN resolves", func(t *testing.T) {
		gotAccount, role, err := ResolveRoleByARN(roles, "arn:aws:iam::000000000000:role/admin")
		require.NoError(t, err)
		assert.Equal(t, account, gotAccount)
		assert.Equal(t, "arn:aws:iam::000000000000:role/admin", aws.StringValue(role.Arn))
	})

	t.Run("a pathed role resolves by its full stored ARN", func(t *testing.T) {
		_, role, err := ResolveRoleByARN(roles, "arn:aws:iam::000000000000:role/team/app-worker")
		require.NoError(t, err)
		assert.Equal(t, "arn:aws:iam::000000000000:role/team/app-worker", aws.StringValue(role.Arn))
	})

	t.Run("an invented path does not reach the role it names", func(t *testing.T) {
		// `app-x/admin` looks up `admin`, whose stored ARN carries no such path.
		_, _, err := ResolveRoleByARN(roles, "arn:aws:iam::000000000000:role/app-x/admin")
		require.ErrorIs(t, err, ErrRoleUnresolved)
	})

	t.Run("a pathed role is not reachable by its pathless ARN", func(t *testing.T) {
		_, _, err := ResolveRoleByARN(roles, "arn:aws:iam::000000000000:role/app-worker")
		require.ErrorIs(t, err, ErrRoleUnresolved)
	})

	t.Run("a role that does not exist is unresolved", func(t *testing.T) {
		_, _, err := ResolveRoleByARN(roles, "arn:aws:iam::000000000000:role/nobody")
		require.ErrorIs(t, err, ErrRoleUnresolved)
	})

	t.Run("a malformed ARN is a validation error", func(t *testing.T) {
		_, _, err := ResolveRoleByARN(roles, "not-an-arn")
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrRoleUnresolved)
		code, ok := awserrors.ResolveErrorCode(err)
		require.True(t, ok)
		assert.Equal(t, awserrors.ErrorValidationError, code)
	})
}
