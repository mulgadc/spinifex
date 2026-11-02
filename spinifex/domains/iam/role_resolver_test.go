package iam

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/passrole"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newResolverFixture(t *testing.T) (*IAMServiceImpl, *RoleResolver, func()) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	masterKey, err := GenerateMasterKey()
	require.NoError(t, err)
	svc, err := NewIAMServiceImpl(t.Context(), nc, masterKey)
	require.NoError(t, err)
	return svc, NewRoleResolver(svc), nc.Close
}

func TestRoleResolver_ResolveRole(t *testing.T) {
	t.Parallel()
	svc, res, _ := newResolverFixture(t)

	for _, in := range []*iam.CreateRoleInput{
		{RoleName: aws.String("plain"), AssumeRolePolicyDocument: aws.String(validTrustPolicy())},
		{RoleName: aws.String("pathed"), Path: aws.String("/svc/"), AssumeRolePolicyDocument: aws.String(validTrustPolicy())},
	} {
		_, err := svc.CreateRole(testAccountID, in)
		require.NoError(t, err)
	}
	acct := testAccountID

	tests := []struct {
		name    string
		arn     string
		want    passrole.Role
		wantErr error
	}{
		{"existing", "arn:aws:iam::" + acct + ":role/plain", passrole.Role{ARN: "arn:aws:iam::" + acct + ":role/plain", Name: "plain"}, nil},
		{"existing with path", "arn:aws:iam::" + acct + ":role/svc/pathed", passrole.Role{ARN: "arn:aws:iam::" + acct + ":role/svc/pathed", Name: "pathed"}, nil},
		{"path omitted is a different spelling", "arn:aws:iam::" + acct + ":role/pathed", passrole.Role{}, passrole.ErrNotFound},
		{"nonexistent", "arn:aws:iam::" + acct + ":role/ghost", passrole.Role{}, passrole.ErrNotFound},
		{"foreign account", "arn:aws:iam::999999999999:role/plain", passrole.Role{}, passrole.ErrForeignAccount},
		{"not an ARN", "plain", passrole.Role{}, passrole.ErrMalformed},
		{"empty", "", passrole.Role{}, passrole.ErrMalformed},
		{"user ARN", "arn:aws:iam::" + acct + ":user/plain", passrole.Role{}, passrole.ErrMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := res.ResolveRole(acct, tt.arn)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRoleResolver_OutageIsNotClassified(t *testing.T) {
	t.Parallel()
	_, res, closeConn := newResolverFixture(t)
	closeConn()
	_, err := res.ResolveRole(testAccountID, "arn:aws:iam::"+testAccountID+":role/plain")
	require.Error(t, err)
	for _, s := range []error{passrole.ErrMalformed, passrole.ErrForeignAccount, passrole.ErrNotFound} {
		assert.NotErrorIs(t, err, s, "an outage must not be classified as %v", s)
	}
}

func TestPassroleCheck_ThroughRealResolver(t *testing.T) {
	t.Parallel()
	svc, res, closeConn := newResolverFixture(t)
	_, err := svc.CreateRole(testAccountID, &iam.CreateRoleInput{
		RoleName: aws.String("worker"), AssumeRolePolicyDocument: aws.String(validTrustPolicy()),
	})
	require.NoError(t, err)
	acct := testAccountID
	arn := "arn:aws:iam::" + acct + ":role/worker"
	denied := errors.New("denied")

	var gotKeys iampolicy.ConditionKeys
	var gotRes []string
	allow := func(_, action string, r []string, k iampolicy.ConditionKeys) error {
		assert.Equal(t, "PassRole", action)
		gotRes, gotKeys = r, k
		return nil
	}
	deny := func(_, _ string, _ []string, _ iampolicy.ConditionKeys) error { return denied }

	role, err := passrole.Check(res, allow, acct, arn, "lambda.amazonaws.com")
	require.NoError(t, err)
	assert.Equal(t, "worker", role.Name)
	assert.Equal(t, []string{arn}, gotRes)
	assert.Equal(t, "lambda.amazonaws.com", gotKeys[iampolicy.KeyPassedToService])

	_, err = passrole.Check(res, deny, acct, arn, "lambda.amazonaws.com")
	assert.ErrorIs(t, err, denied)

	// A denied caller gets the denial, not NotFound, for a role that is absent.
	ghost := "arn:aws:iam::" + acct + ":role/ghost"
	_, err = passrole.Check(res, deny, acct, ghost, "lambda.amazonaws.com")
	assert.ErrorIs(t, err, denied)
	_, err = passrole.Check(res, allow, acct, ghost, "lambda.amazonaws.com")
	assert.ErrorIs(t, err, passrole.ErrNotFound)

	// Foreign and malformed references never reach authorization.
	called := false
	spy := func(_, _ string, _ []string, _ iampolicy.ConditionKeys) error { called = true; return nil }
	_, err = passrole.Check(res, spy, acct, "arn:aws:iam::999999999999:role/worker", "lambda.amazonaws.com")
	require.ErrorIs(t, err, passrole.ErrForeignAccount)
	_, err = passrole.Check(res, spy, acct, "worker", "lambda.amazonaws.com")
	require.ErrorIs(t, err, passrole.ErrMalformed)
	assert.False(t, called)

	// An outage is returned unclassified with nothing authorized.
	closeConn()
	_, err = passrole.Check(res, spy, acct, arn, "lambda.amazonaws.com")
	require.Error(t, err)
	assert.NotErrorIs(t, err, passrole.ErrNotFound)
	assert.False(t, called)
}
