package handlers_sts

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetSessionToken_HappyPath_MintsUserSession(t *testing.T) {
	svc, _ := newTestSetup(t)

	out, err := svc.GetSessionToken(testCallerAccountID, testCallerUserName, principalTypeUser, testCallerAccessKeyID,
		&sts.GetSessionTokenInput{})
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.Credentials)

	akid := aws.StringValue(out.Credentials.AccessKeyId)
	secret := aws.StringValue(out.Credentials.SecretAccessKey)
	token := aws.StringValue(out.Credentials.SessionToken)

	assert.True(t, strings.HasPrefix(akid, SessionAccessKeyIDPrefix))
	assert.Len(t, akid, 20)
	assert.NotEmpty(t, secret)
	assert.NotEmpty(t, token)
	assert.True(t, out.Credentials.Expiration.After(time.Now().UTC()))

	stored, err := svc.LookupSessionCredential(akid)
	require.NoError(t, err)
	require.NotNil(t, stored)

	// The defining property of a GetSessionToken session: it is user-bound, so
	// PrincipalType is "user", SessionName is the IAM user name, and every
	// assumed-role field is empty. resolveSessionAKID relies on exactly this.
	assert.Equal(t, principalTypeUser, stored.PrincipalType)
	assert.Equal(t, testCallerUserName, stored.SessionName)
	assert.Equal(t, testCallerAccountID, stored.AccountID)
	assert.Empty(t, stored.AssumedRoleARN)
	assert.Empty(t, stored.UnderlyingRoleARN)
	assert.Empty(t, stored.RoleID)
	assert.Empty(t, stored.AssumedRoleID)
	assert.Empty(t, stored.SourceIdentity)

	// The wire token must HMAC to the persisted record, and the persisted secret
	// must decrypt back — same minting invariants as AssumeRole.
	mac := hmac.New(sha256.New, svc.masterKey)
	mac.Write([]byte(token))
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	assert.Equal(t, expected, stored.SessionTokenHMAC)
	assert.NotEqual(t, token, stored.SessionTokenHMAC, "raw token must not be persisted")

	decrypted, err := handlers_iam.DecryptSecret(stored.SecretEncrypted, svc.masterKey)
	require.NoError(t, err)
	assert.Equal(t, secret, decrypted)
}

func TestGetSessionToken_NilInput_DefaultsToTwelveHours(t *testing.T) {
	svc, _ := newTestSetup(t)

	out, err := svc.GetSessionToken(testCallerAccountID, testCallerUserName, principalTypeUser, testCallerAccessKeyID, nil)
	require.NoError(t, err)
	require.NotNil(t, out.Credentials)

	assertStoredDuration(t, svc, out, getSessionTokenDefaultDuration)
}

func TestGetSessionToken_DurationClamp(t *testing.T) {
	svc, _ := newTestSetup(t)

	cases := []struct {
		name      string
		requested int64
		want      int64
	}{
		{"below_minimum_clamps_up", minDurationSeconds - 1, minDurationSeconds},
		{"at_minimum", minDurationSeconds, minDurationSeconds},
		{"in_range", 7200, 7200},
		{"at_maximum", getSessionTokenMaxDuration, getSessionTokenMaxDuration},
		{"above_maximum_clamps_down", getSessionTokenMaxDuration + 1, getSessionTokenMaxDuration},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := svc.GetSessionToken(testCallerAccountID, testCallerUserName, principalTypeUser, testCallerAccessKeyID,
				&sts.GetSessionTokenInput{DurationSeconds: aws.Int64(tc.requested)})
			require.NoError(t, err)
			assertStoredDuration(t, svc, out, tc.want)
		})
	}
}

func TestGetSessionToken_RejectsNonUserAndSessionCallers(t *testing.T) {
	svc, _ := newTestSetup(t)

	// Only a long-lived user (AKIA + principalType "user") may call
	// GetSessionToken. Assumed-role callers are denied by principal type;
	// session callers are denied by their ASIA access-key prefix — including a
	// GetSessionToken session, which resolves back to principalType "user" and
	// would otherwise roll its own lifetime forward indefinitely.
	cases := []struct {
		name          string
		principalType string
		accessKeyID   string
	}{
		{"assumed_role_session", principalTypeAssumedRole, "ASIAEXAMPLEAAAAAAAAA"},
		{"user_session", principalTypeUser, "ASIAEXAMPLEAAAAAAAAA"},
		{"empty_principal_type", "", testCallerAccessKeyID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := svc.GetSessionToken(testCallerAccountID, testCallerUserName, tc.principalType, tc.accessKeyID,
				&sts.GetSessionTokenInput{})
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
			assert.Nil(t, out)
		})
	}
}

func TestGetSessionToken_RejectsMFAParameters(t *testing.T) {
	svc, _ := newTestSetup(t)

	cases := []struct {
		name  string
		input *sts.GetSessionTokenInput
	}{
		{"serial_number", &sts.GetSessionTokenInput{SerialNumber: aws.String("arn:aws:iam::000000000000:mfa/alice")}},
		{"token_code", &sts.GetSessionTokenInput{TokenCode: aws.String("123456")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := svc.GetSessionToken(testCallerAccountID, testCallerUserName, principalTypeUser, testCallerAccessKeyID, tc.input)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
			assert.Nil(t, out)
		})
	}
}

func TestGetSessionToken_RejectsMissingUserName(t *testing.T) {
	svc, _ := newTestSetup(t)

	out, err := svc.GetSessionToken(testCallerAccountID, "", principalTypeUser, testCallerAccessKeyID, &sts.GetSessionTokenInput{})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
	assert.Nil(t, out)
}

// assertStoredDuration looks up the minted credential and asserts the persisted
// lifetime (ExpiresAt - CreatedAt) equals the expected number of seconds. This
// reads the clamp through the stored record rather than re-deriving it.
func assertStoredDuration(t *testing.T, svc *STSServiceImpl, out *sts.GetSessionTokenOutput, wantSeconds int64) {
	t.Helper()
	stored, err := svc.LookupSessionCredential(aws.StringValue(out.Credentials.AccessKeyId))
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, time.Duration(wantSeconds)*time.Second, stored.ExpiresAt.Sub(stored.CreatedAt))
}
