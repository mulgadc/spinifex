package credverify

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testRoleID      = "AROAROLEID0000000001"
	testAssumedARN  = "arn:aws:sts::000000000001:assumed-role/SharedOps/session"
	testRoleARNLive = "arn:aws:iam::000000000001:role/SharedOps"
)

// roleIAM answers GetRole for the one role name with the given ID, or as deleted
// when id is empty.
func roleIAM(id string) *fakeIAM {
	return &fakeIAM{getRoleFn: func(_ string, in *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
		if id == "" || aws.StringValue(in.RoleName) != "SharedOps" {
			return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
		}
		return &iam.GetRoleOutput{Role: &iam.Role{
			RoleName: aws.String("SharedOps"), RoleId: aws.String(id), Arn: aws.String(testRoleARNLive),
		}}, nil
	}}
}

// A role session that has expired or been swept is rebuilt from the live role, and
// must yield what the session itself yielded while it lived.
func TestRehydrateRole_LiveRoleYieldsTheSessionPrincipal(t *testing.T) {
	cred := roleSession(testRoleID + ":session")
	cred.RoleID = testRoleID
	fromSession, f := New(&fakeIAM{}, &fakeSTS{cred: cred}).Rehydrate(testSession, testAccount)
	require.Nil(t, f)

	fromRole, f := New(roleIAM(testRoleID), nil).RehydrateRole(testAccount, testRoleID, testAssumedARN)
	require.Nil(t, f)
	assert.Equal(t, fromSession, fromRole)
	assert.Equal(t, testRoleID+":session", fromRole.AssumedRoleID)
	assert.Equal(t, testAssumedARN, fromRole.CallerARN)
}

func TestRehydrateRole_RoleGone(t *testing.T) {
	cases := map[string]*fakeIAM{
		"deleted":                  roleIAM(""),
		"recreated under a new ID": roleIAM("AROAOTHERROLEID00002"),
	}
	for name, iamSvc := range cases {
		t.Run(name, func(t *testing.T) {
			_, f := New(iamSvc, nil).RehydrateRole(testAccount, testRoleID, testAssumedARN)
			require.NotNil(t, f)
			assert.True(t, f.RoleGone, "a missing or replaced role is distinct from a fault")
			assert.Equal(t, awserrors.ErrorInvalidClientTokenId, f.Code)
		})
	}
}

func TestRehydrateRole_FaultsAreNotRoleGone(t *testing.T) {
	outage := &fakeIAM{getRoleFn: func(string, *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
		return nil, errors.New("nats down")
	}}
	cases := map[string]struct {
		v          *Verifier
		accountID  string
		roleID     string
		assumedARN string
	}{
		"IAM outage":     {New(outage, nil), testAccount, testRoleID, testAssumedARN},
		"nil verifier":   {nil, testAccount, testRoleID, testAssumedARN},
		"empty role ID":  {New(roleIAM(testRoleID), nil), testAccount, "", testAssumedARN},
		"foreign ARN":    {New(roleIAM(testRoleID), nil), "000000000002", testRoleID, testAssumedARN},
		"not a role ARN": {New(roleIAM(testRoleID), nil), testAccount, testRoleID, "arn:aws:iam::000000000001:user/dev"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, f := tc.v.RehydrateRole(tc.accountID, tc.roleID, tc.assumedARN)
			require.NotNil(t, f)
			assert.False(t, f.RoleGone)
		})
	}
}
