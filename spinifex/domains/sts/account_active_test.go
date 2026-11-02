package sts

import (
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sts"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// accountReadErrIAM fails the account read and delegates everything else.
type accountReadErrIAM struct {
	iamdomain.IAMService
}

func (accountReadErrIAM) GetAccount(string) (*iamdomain.Account, error) {
	return nil, errors.New("kv unavailable")
}

// Writing the record directly covers states SetAccountStatus will not produce,
// and a missing record is simulated by a status the gate cannot accept.
var notActiveStates = []struct {
	name   string
	status string
}{
	{"terminating", iamdomain.AccountStatusTerminating},
	{"suspended", iamdomain.AccountStatusSuspended},
	{"unknown status", "CLOSED"},
}

// sessionCount counts stored session credentials, ignoring the bucket's
// internal version marker.
func sessionCount(t *testing.T, svc *STSServiceImpl, nc *nats.Conn) int {
	t.Helper()
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.KeyValue(t.Context(), "spinifex-iam-session-credentials")
	require.NoError(t, err)
	keys, err := kv.Keys(t.Context())
	if errors.Is(err, jetstream.ErrNoKeysFound) {
		return 0
	}
	require.NoError(t, err)
	n := 0
	for _, k := range keys {
		if !strings.HasPrefix(k, "_") {
			n++
		}
	}
	return n
}

func requireAccessDenied(t *testing.T, err error, wantMessage string) {
	t.Helper()
	code, msg, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok, "unresolved error: %v", err)
	assert.Equal(t, awserrors.ErrorAccessDenied, code)
	assert.Equal(t, wantMessage, msg)
}

func TestAssumeRole_AccountNotActive_RefusedLikeTrustDenial(t *testing.T) {
	for _, tc := range notActiveStates {
		t.Run(tc.name, func(t *testing.T) {
			svc, nc := newTestSetup(t)
			role := createRoleInAccount(t, svc, testCallerAccountID, "app", trustPolicyAllowingUser(testCallerARN()))
			putTestAccount(t, nc, testCallerAccountID, tc.status)

			out, err := svc.AssumeRole(testCallerAccountID, testCallerARN(), testCallerUserName,
				basicAssumeRoleInput(*role.Arn, "sess"))
			require.Error(t, err)
			assert.Nil(t, out)
			requireAccessDenied(t, err, assumeRoleDeniedMessage(testCallerARN(), *role.Arn))
			assert.Zero(t, sessionCount(t, svc, nc))
		})
	}
}

func TestAssumeRole_AccountRecordMissing_Refused(t *testing.T) {
	svc, nc := newTestSetup(t)
	role := createRoleInAccount(t, svc, testCallerAccountID, "app", trustPolicyAllowingUser(testCallerARN()))
	deleteTestAccount(t, nc, testCallerAccountID)

	_, err := svc.AssumeRole(testCallerAccountID, testCallerARN(), testCallerUserName,
		basicAssumeRoleInput(*role.Arn, "sess"))
	requireAccessDenied(t, err, assumeRoleDeniedMessage(testCallerARN(), *role.Arn))
	assert.Zero(t, sessionCount(t, svc, nc))
}

// A refusal for an inactive account must not be distinguishable from the one a
// caller gets for a role whose trust policy does not admit them.
func TestAssumeRole_AccountNotActive_SameErrorAsTrustPolicyDenial(t *testing.T) {
	svc, nc := newTestSetup(t)
	other := "arn:aws:iam::" + testCallerAccountID + ":user/someone-else"
	denied := createRoleInAccount(t, svc, testCallerAccountID, "denied", trustPolicyAllowingUser(other))
	inactive := createRoleInAccount(t, svc, testCrossAccountID, "inactive", trustPolicyAllowingUser(testCallerARN()))
	putTestAccount(t, nc, testCrossAccountID, iamdomain.AccountStatusTerminating)

	_, trustErr := svc.AssumeRole(testCallerAccountID, testCallerARN(), testCallerUserName,
		basicAssumeRoleInput(*denied.Arn, "sess"))
	_, inactiveErr := svc.AssumeRole(testCallerAccountID, testCallerARN(), testCallerUserName,
		basicAssumeRoleInput(*inactive.Arn, "sess"))

	trustCode, trustMsg, _ := awserrors.ResolveErrorDetail(trustErr)
	inactiveCode, inactiveMsg, _ := awserrors.ResolveErrorDetail(inactiveErr)
	assert.Equal(t, trustCode, inactiveCode)
	assert.Equal(t, assumeRoleDeniedMessage(testCallerARN(), *denied.Arn), trustMsg)
	assert.Equal(t, assumeRoleDeniedMessage(testCallerARN(), *inactive.Arn), inactiveMsg)
}

// The caller's own account is ACTIVE; the role's account decides.
func TestAssumeRole_CrossAccountIntoNotActiveAccount_Refused(t *testing.T) {
	for _, tc := range notActiveStates {
		t.Run(tc.name, func(t *testing.T) {
			svc, nc := newTestSetup(t)
			role := createRoleInAccount(t, svc, testCrossAccountID, "shared", trustPolicyAllowingUser(testCallerARN()))

			_, err := svc.AssumeRole(testCallerAccountID, testCallerARN(), testCallerUserName,
				basicAssumeRoleInput(*role.Arn, "sess"))
			require.NoError(t, err, "role account is ACTIVE, so the call mints")
			before := sessionCount(t, svc, nc)

			putTestAccount(t, nc, testCrossAccountID, tc.status)
			_, err = svc.AssumeRole(testCallerAccountID, testCallerARN(), testCallerUserName,
				basicAssumeRoleInput(*role.Arn, "sess"))
			requireAccessDenied(t, err, assumeRoleDeniedMessage(testCallerARN(), *role.Arn))
			assert.Equal(t, before, sessionCount(t, svc, nc))
		})
	}
}

func TestAssumeRole_AccountReadError_InternalError(t *testing.T) {
	svc, nc := newTestSetup(t)
	role := createRoleInAccount(t, svc, testCallerAccountID, "app", trustPolicyAllowingUser(testCallerARN()))
	svc.iamSvc = accountReadErrIAM{IAMService: svc.iamSvc}

	_, err := svc.AssumeRole(testCallerAccountID, testCallerARN(), testCallerUserName,
		basicAssumeRoleInput(*role.Arn, "sess"))
	code, ok := awserrors.ResolveErrorCode(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorInternalError, code)
	assert.Zero(t, sessionCount(t, svc, nc))
}

func TestAssumeRoleForInstance_AccountNotActive_BareAccessDenied(t *testing.T) {
	for _, tc := range notActiveStates {
		t.Run(tc.name, func(t *testing.T) {
			svc, nc := newTestSetup(t)
			role := createRoleInAccount(t, svc, testCallerAccountID, "inst", trustPolicyAllowingEC2Service())

			_, err := svc.AssumeRoleForInstance(testCallerAccountID, *role.Arn, "i-abc", 3600)
			require.NoError(t, err, "ACTIVE account mints")
			before := sessionCount(t, svc, nc)

			putTestAccount(t, nc, testCallerAccountID, tc.status)
			_, err = svc.AssumeRoleForInstance(testCallerAccountID, *role.Arn, "i-abc", 3600)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
			assert.Equal(t, before, sessionCount(t, svc, nc))
		})
	}
}

func TestGetSessionToken_AccountNotActive_AccessDenied(t *testing.T) {
	for _, tc := range notActiveStates {
		t.Run(tc.name, func(t *testing.T) {
			svc, nc := newTestSetup(t)
			seedUser(t, svc, testCallerAccountID, testCallerUserName)
			putTestAccount(t, nc, testCallerAccountID, tc.status)

			out, err := svc.GetSessionToken(testCallerAccountID, testCallerUserName, principalTypeUser,
				testCallerAccessKeyID, &sts.GetSessionTokenInput{})
			assert.Nil(t, out)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
			assert.Zero(t, sessionCount(t, svc, nc))
		})
	}
}

func TestAssumeRoleWithWebIdentity_AccountNotActive_AccessDenied(t *testing.T) {
	for _, tc := range notActiveStates {
		t.Run(tc.name, func(t *testing.T) {
			svc, nc := newTestSetup(t)
			f := newWebIdentityFixture(t, svc, testCallerAccountID)
			role := createRoleForFixture(t, svc, testCallerAccountID, "irsa-app",
				webIdentityTrustPolicy(f.federatedARN, f.issuer, "system:serviceaccount:default:my-sa"))
			putTestAccount(t, nc, testCallerAccountID, tc.status)

			out, err := svc.AssumeRoleWithWebIdentity(&sts.AssumeRoleWithWebIdentityInput{
				RoleArn:          role.Arn,
				RoleSessionName:  aws.String("pod-1"),
				WebIdentityToken: aws.String(f.signToken(f.defaultClaims())),
			})
			assert.Nil(t, out)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
			assert.Zero(t, sessionCount(t, svc, nc))
		})
	}
}

// An invalid token is rejected before account state is read, so an
// unauthenticated caller learns nothing about the account.
func TestAssumeRoleWithWebIdentity_BadTokenNotActive_StillInvalidToken(t *testing.T) {
	svc, nc := newTestSetup(t)
	f := newWebIdentityFixture(t, svc, testCallerAccountID)
	role := createRoleForFixture(t, svc, testCallerAccountID, "irsa-app",
		webIdentityTrustPolicy(f.federatedARN, f.issuer, "system:serviceaccount:default:my-sa"))
	putTestAccount(t, nc, testCallerAccountID, iamdomain.AccountStatusTerminating)

	_, err := svc.AssumeRoleWithWebIdentity(&sts.AssumeRoleWithWebIdentityInput{
		RoleArn:          role.Arn,
		RoleSessionName:  aws.String("pod-1"),
		WebIdentityToken: aws.String("not-a-jwt"),
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidIdentityToken, err.Error())
}
