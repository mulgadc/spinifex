// behind the unexported STSServiceImpl.iamSvc, which the fault cases swap out,
// and the credential fixtures set unexported principal-type constants.
//
//test:in-package — the verdicts are asserted against the real IAM backend
package handlers_sts

import (
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedUser creates an IAM user and returns its immutable UserId.
func seedUser(t *testing.T, svc *STSServiceImpl, accountID, userName string) string {
	t.Helper()
	out, err := svc.iamSvc.CreateUser(accountID, &iam.CreateUserInput{UserName: aws.String(userName)})
	require.NoError(t, err)
	userID := aws.StringValue(out.User.UserId)
	require.NotEmpty(t, userID)
	return userID
}

func deleteUser(t *testing.T, svc *STSServiceImpl, accountID, userName string) {
	t.Helper()
	_, err := svc.iamSvc.DeleteUser(accountID, &iam.DeleteUserInput{UserName: aws.String(userName)})
	require.NoError(t, err)
}

func deleteRole(t *testing.T, svc *STSServiceImpl, accountID, roleName string) {
	t.Helper()
	_, err := svc.iamSvc.DeleteRole(accountID, &iam.DeleteRoleInput{RoleName: aws.String(roleName)})
	require.NoError(t, err)
}

// mintUserSession runs the real GetSessionToken path and returns the persisted record.
func mintUserSession(t *testing.T, svc *STSServiceImpl, accountID, userName string) *SessionCredential {
	t.Helper()
	out, err := svc.GetSessionToken(accountID, userName, principalTypeUser, testCallerAccessKeyID,
		&sts.GetSessionTokenInput{})
	require.NoError(t, err)
	cred, err := svc.LookupSessionCredential(aws.StringValue(out.Credentials.AccessKeyId))
	require.NoError(t, err)
	require.NotNil(t, cred)
	return cred
}

// mintRoleSession runs the real AssumeRole path and returns the persisted record.
func mintRoleSession(t *testing.T, svc *STSServiceImpl, accountID, roleARN, sessionName string) *SessionCredential {
	t.Helper()
	out, err := svc.AssumeRole(accountID, testCallerARN(), testCallerUserName,
		basicAssumeRoleInput(roleARN, sessionName))
	require.NoError(t, err)
	cred, err := svc.LookupSessionCredential(aws.StringValue(out.Credentials.AccessKeyId))
	require.NoError(t, err)
	require.NotNil(t, cred)
	return cred
}

// ----- Unchanged principals verify ---------------------------------------

func TestVerifySessionPrincipal_UnchangedUser_ReturnsLiveID(t *testing.T) {
	svc, _ := newTestSetup(t)
	userID := seedUser(t, svc, testCallerAccountID, testCallerUserName)
	cred := mintUserSession(t, svc, testCallerAccountID, testCallerUserName)

	got, err := svc.VerifySessionPrincipal(cred)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, userID, got.UserID)
	assert.Empty(t, got.RoleID)
}

func TestVerifySessionPrincipal_UnchangedRole_ReturnsLiveRecord(t *testing.T) {
	svc, _ := newTestSetup(t)
	role := createRoleInAccount(t, svc, testCallerAccountID, "app", trustPolicyAllowingUser(testCallerARN()))
	cred := mintRoleSession(t, svc, testCallerAccountID, aws.StringValue(role.Arn), "session-1")

	got, err := svc.VerifySessionPrincipal(cred)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, aws.StringValue(role.RoleId), got.RoleID)
	assert.Equal(t, "app", got.RoleName)
	assert.Equal(t, aws.StringValue(role.Arn), got.RoleARN)
}

// ----- Same-name recreation: the defect this check closes -----------------

// The reported attack: an administrator deletes a user and recreates it under
// the same name, and the deleted user's unexpired session is authorized with
// the replacement's permissions. The replacement's UserId differs, so the
// session must die on that.
func TestVerifySessionPrincipal_UserRecreatedSameName_Rejected(t *testing.T) {
	svc, _ := newTestSetup(t)
	originalID := seedUser(t, svc, testCallerAccountID, testCallerUserName)
	cred := mintUserSession(t, svc, testCallerAccountID, testCallerUserName)

	deleteUser(t, svc, testCallerAccountID, testCallerUserName)
	replacementID := seedUser(t, svc, testCallerAccountID, testCallerUserName)
	require.NotEqual(t, originalID, replacementID, "recreation must change the immutable ID")

	// The replacement is granted a permission the original never had; the old
	// session must not reach the policy evaluator at all.
	_, err := svc.iamSvc.PutUserPolicy(testCallerAccountID, &iam.PutUserPolicyInput{
		UserName:       aws.String(testCallerUserName),
		PolicyName:     aws.String("list-users"),
		PolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"iam:ListUsers","Resource":"*"}]}`),
	})
	require.NoError(t, err)

	got, err := svc.VerifySessionPrincipal(cred)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSessionPrincipalReplaced)
	assert.Nil(t, got)
}

func TestVerifySessionPrincipal_RoleRecreatedSameName_Rejected(t *testing.T) {
	svc, _ := newTestSetup(t)
	caller := testCallerARN()
	original := createRoleInAccount(t, svc, testCallerAccountID, "app", trustPolicyAllowingUser(caller))
	cred := mintRoleSession(t, svc, testCallerAccountID, aws.StringValue(original.Arn), "session-1")

	deleteRole(t, svc, testCallerAccountID, "app")
	replacement := createRoleInAccount(t, svc, testCallerAccountID, "app", trustPolicyAllowingWildcard())
	require.NotEqual(t, aws.StringValue(original.RoleId), aws.StringValue(replacement.RoleId))
	// Delete-and-recreate at the same name and path reproduces the ARN byte for
	// byte, so the ARN comparison alone would not catch this.
	require.Equal(t, aws.StringValue(original.Arn), aws.StringValue(replacement.Arn))

	_, err := svc.iamSvc.PutRolePolicy(testCallerAccountID, &iam.PutRolePolicyInput{
		RoleName:       aws.String("app"),
		PolicyName:     aws.String("admin"),
		PolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`),
	})
	require.NoError(t, err)

	got, err := svc.VerifySessionPrincipal(cred)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSessionPrincipalReplaced)
	assert.Nil(t, got)
}

// A replacement role whose trust policy excludes the original caller must not
// keep serving that caller's session. Trust is evaluated once, at AssumeRole,
// so the ID mismatch is the only thing standing between the two.
func TestVerifySessionPrincipal_RoleReplacedWithExcludingTrustPolicy_Rejected(t *testing.T) {
	svc, _ := newTestSetup(t)
	original := createRoleInAccount(t, svc, testCallerAccountID, "app", trustPolicyAllowingUser(testCallerARN()))
	cred := mintRoleSession(t, svc, testCallerAccountID, aws.StringValue(original.Arn), "session-1")

	deleteRole(t, svc, testCallerAccountID, "app")
	createRoleInAccount(t, svc, testCallerAccountID, "app",
		trustPolicyAllowingUser("arn:aws:iam::"+testCallerAccountID+":user/someone-else"))

	_, err := svc.VerifySessionPrincipal(cred)
	assert.ErrorIs(t, err, ErrSessionPrincipalReplaced)
}

// ----- Deleted and not recreated -----------------------------------------

func TestVerifySessionPrincipal_UserDeleted_Gone(t *testing.T) {
	svc, _ := newTestSetup(t)
	seedUser(t, svc, testCallerAccountID, testCallerUserName)
	cred := mintUserSession(t, svc, testCallerAccountID, testCallerUserName)

	deleteUser(t, svc, testCallerAccountID, testCallerUserName)

	_, err := svc.VerifySessionPrincipal(cred)
	assert.ErrorIs(t, err, ErrSessionPrincipalGone)
}

func TestVerifySessionPrincipal_RoleDeleted_Gone(t *testing.T) {
	svc, _ := newTestSetup(t)
	role := createRoleInAccount(t, svc, testCallerAccountID, "app", trustPolicyAllowingUser(testCallerARN()))
	cred := mintRoleSession(t, svc, testCallerAccountID, aws.StringValue(role.Arn), "session-1")

	deleteRole(t, svc, testCallerAccountID, "app")

	_, err := svc.VerifySessionPrincipal(cred)
	assert.ErrorIs(t, err, ErrSessionPrincipalGone)
}

// A session whose stored underlying role ARN names another account resolves to
// no role this account can vouch for, so it must not verify.
func TestVerifySessionPrincipal_CrossAccountRoleARN_Rejected(t *testing.T) {
	svc, _ := newTestSetup(t)
	role := createRoleInAccount(t, svc, testCallerAccountID, "app", trustPolicyAllowingUser(testCallerARN()))
	cred := mintRoleSession(t, svc, testCallerAccountID, aws.StringValue(role.Arn), "session-1")
	cred.UnderlyingRoleARN = "arn:aws:iam::" + testCrossAccountID + ":role/app"

	_, err := svc.VerifySessionPrincipal(cred)
	require.Error(t, err)
	assert.True(t, IsSessionPrincipalVerdict(err))
}

// ----- Legacy records fail closed ----------------------------------------

// Records minted before the immutable ID was persisted carry nothing to compare,
// and are rejected rather than waved through: "absent evidence passes" preserves
// exactly the gap this check closes, and would swallow any future mint path that
// failed to populate the field.
func TestVerifySessionPrincipal_LegacyRecord_Rejected(t *testing.T) {
	svc, _ := newTestSetup(t)
	seedUser(t, svc, testCallerAccountID, testCallerUserName)
	role := createRoleInAccount(t, svc, testCallerAccountID, "app", trustPolicyAllowingUser(testCallerARN()))

	cases := []struct {
		name string
		cred *SessionCredential
	}{
		{"user session with no UserID", &SessionCredential{
			AccessKeyID:   "ASIALEGACYUSER000001",
			AccountID:     testCallerAccountID,
			PrincipalType: principalTypeUser,
			SessionName:   testCallerUserName,
		}},
		{"role session with no RoleID", &SessionCredential{
			AccessKeyID:       "ASIALEGACYROLE000001",
			AccountID:         testCallerAccountID,
			PrincipalType:     principalTypeAssumedRole,
			SessionName:       "session-1",
			UnderlyingRoleARN: aws.StringValue(role.Arn),
		}},
		{"pre-PrincipalType role record", &SessionCredential{
			AccessKeyID:       "ASIALEGACYEMPTY00001",
			AccountID:         testCallerAccountID,
			SessionName:       "session-1",
			UnderlyingRoleARN: aws.StringValue(role.Arn),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := svc.VerifySessionPrincipal(tc.cred)
			assert.ErrorIs(t, err, ErrSessionPrincipalLegacy)
			assert.Nil(t, got)
		})
	}
}

// ----- Root -------------------------------------------------------------

// Root sessions skip policy evaluation entirely, so continuity is the only gate
// on one. No special case is needed: the seeded record's constant ID matches
// while root exists, and its removal fails closed like any other principal.
func TestVerifySessionPrincipal_Root(t *testing.T) {
	svc, _ := newTestSetup(t)
	rootID := seedUser(t, svc, testCallerAccountID, "root")
	cred := mintUserSession(t, svc, testCallerAccountID, "root")

	got, err := svc.VerifySessionPrincipal(cred)
	require.NoError(t, err)
	assert.Equal(t, rootID, got.UserID)

	deleteUser(t, svc, testCallerAccountID, "root")

	_, err = svc.VerifySessionPrincipal(cred)
	assert.ErrorIs(t, err, ErrSessionPrincipalGone)
}

// ----- Faults are distinguishable from verdicts --------------------------

// A dependency fault must not read as a rejection verdict: the doors answer
// InternalError for the first and a token error for the second, and collapsing
// them would let an IAM outage look like a revoked principal.
func TestVerifySessionPrincipal_DependencyFault_NotAVerdict(t *testing.T) {
	svc, _ := newTestSetup(t)
	boom := errors.New("jetstream unavailable")
	svc.iamSvc = faultingIAMService{err: boom}

	cred := &SessionCredential{
		AccessKeyID:   "ASIAFAULTUSER0000001",
		AccountID:     testCallerAccountID,
		PrincipalType: principalTypeUser,
		SessionName:   testCallerUserName,
		UserID:        "AIDAEXAMPLEAAAAAAAAA",
		ExpiresAt:     time.Now().UTC().Add(time.Hour),
	}

	got, err := svc.VerifySessionPrincipal(cred)
	require.Error(t, err)
	assert.Nil(t, got)
	assert.ErrorIs(t, err, boom)
	assert.False(t, IsSessionPrincipalVerdict(err), "a fault must not read as a rejection verdict")
}

func TestVerifySessionPrincipal_NilCredential_IsAFault(t *testing.T) {
	svc, _ := newTestSetup(t)

	got, err := svc.VerifySessionPrincipal(nil)
	require.Error(t, err)
	assert.Nil(t, got)
	assert.False(t, IsSessionPrincipalVerdict(err))
}

// faultingIAMService fails every lookup with a non-NoSuchEntity error, standing
// in for an unreachable IAM backend.
type faultingIAMService struct {
	handlers_iam.IAMService

	err error
}

func (f faultingIAMService) GetUser(string, *iam.GetUserInput) (*iam.GetUserOutput, error) {
	return nil, f.err
}

func (f faultingIAMService) GetRole(string, *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	return nil, f.err
}
