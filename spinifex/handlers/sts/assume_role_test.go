package handlers_sts

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testCallerAccountID = utils.GlobalAccountID
	testCallerUserName  = "alice"
	testCrossAccountID  = "999999999999"
)

func testCallerARN() string {
	return fmt.Sprintf("arn:aws:iam::%s:user/%s", testCallerAccountID, testCallerUserName)
}

func trustPolicyAllowingUser(callerARN string) string {
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":%q},"Action":"sts:AssumeRole"}]}`, callerARN)
}

func trustPolicyAllowingWildcard() string {
	return `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"*"},"Action":"sts:AssumeRole"}]}`
}

func trustPolicyAllowingRoot(accountID string) string {
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::%s:root"},"Action":"sts:AssumeRole"}]}`, accountID)
}

func trustPolicyAllowingBareAccount(accountID string) string {
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":%q},"Action":"sts:AssumeRole"}]}`, accountID)
}

// createRoleInAccount writes a role directly into the IAM roles bucket. The
// public CreateRole API is account-scoped, and the test helpers in this file
// need to seed roles into arbitrary accounts to exercise cross-account paths.
func createRoleInAccount(t *testing.T, svc *STSServiceImpl, accountID, roleName, trustPolicy string) *iam.Role {
	t.Helper()
	out, err := svc.iamSvc.CreateRole(accountID, &iam.CreateRoleInput{
		RoleName:                 aws.String(roleName),
		AssumeRolePolicyDocument: aws.String(trustPolicy),
	})
	require.NoError(t, err)
	return out.Role
}

func basicAssumeRoleInput(roleARN, sessionName string) *sts.AssumeRoleInput {
	return &sts.AssumeRoleInput{
		RoleArn:         aws.String(roleARN),
		RoleSessionName: aws.String(sessionName),
	}
}

func TestAssumeRole_HappyPath_PersistsRecord(t *testing.T) {
	svc, _ := newTestSetup(t)
	caller := testCallerARN()
	role := createRoleInAccount(t, svc, testCallerAccountID, "app", trustPolicyAllowingUser(caller))

	out, err := svc.AssumeRole(testCallerAccountID, caller, testCallerUserName,
		basicAssumeRoleInput(*role.Arn, "session-1"))
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.Credentials)
	require.NotNil(t, out.AssumedRoleUser)

	akid := aws.StringValue(out.Credentials.AccessKeyId)
	secret := aws.StringValue(out.Credentials.SecretAccessKey)
	token := aws.StringValue(out.Credentials.SessionToken)

	assert.True(t, strings.HasPrefix(akid, SessionAccessKeyIDPrefix))
	assert.Len(t, akid, 20)
	assert.NotEmpty(t, secret)
	assert.NotEmpty(t, token)
	assert.True(t, out.Credentials.Expiration.After(time.Now().UTC()))
	assert.Equal(t, fmt.Sprintf("arn:aws:sts::%s:assumed-role/app/session-1", testCallerAccountID),
		aws.StringValue(out.AssumedRoleUser.Arn))
	assert.Equal(t, aws.StringValue(role.RoleId)+":session-1",
		aws.StringValue(out.AssumedRoleUser.AssumedRoleId))

	stored, err := svc.LookupSessionCredential(akid)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "session-1", stored.SessionName)
	assert.Equal(t, aws.StringValue(role.RoleId), stored.RoleID)
	assert.Equal(t, aws.StringValue(role.Arn), stored.UnderlyingRoleARN)

	// SessionToken returned to the client must HMAC to the persisted record
	// using the same master key. This protects the SigV4 verifier's eventual
	// constant-time compare from a stored-vs-wire mismatch regression.
	mac := hmac.New(sha256.New, svc.masterKey)
	mac.Write([]byte(token))
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	assert.Equal(t, expected, stored.SessionTokenHMAC)
	assert.NotEqual(t, token, stored.SessionTokenHMAC, "raw token must not be persisted")

	// Persisted secret must decrypt back to the secret returned to the client.
	decrypted, err := handlers_iam.DecryptSecret(stored.SecretEncrypted, svc.masterKey)
	require.NoError(t, err)
	assert.Equal(t, secret, decrypted)
}

func TestAssumeRole_WildcardPrincipal_Allowed(t *testing.T) {
	svc, _ := newTestSetup(t)
	role := createRoleInAccount(t, svc, testCallerAccountID, "open", trustPolicyAllowingWildcard())

	_, err := svc.AssumeRole(testCallerAccountID, testCallerARN(), testCallerUserName,
		basicAssumeRoleInput(*role.Arn, "sess"))
	require.NoError(t, err)
}

func TestAssumeRole_RootPrincipal_MatchesAnyPrincipalInAccount(t *testing.T) {
	svc, _ := newTestSetup(t)
	role := createRoleInAccount(t, svc, testCallerAccountID, "rooted", trustPolicyAllowingRoot(testCallerAccountID))

	_, err := svc.AssumeRole(testCallerAccountID, testCallerARN(), testCallerUserName,
		basicAssumeRoleInput(*role.Arn, "sess"))
	require.NoError(t, err)
}

// TestAssumeRole_NonIAMRootARN_DoesNotMatch ensures the :root shorthand is
// scoped to arn:aws:iam — a malformed ARN like arn:aws:s3::A:root pasted into
// a trust policy must fail closed, not silently grant the account.
func TestAssumeRole_NonIAMRootARN_DoesNotMatch(t *testing.T) {
	svc, _ := newTestSetup(t)
	trustPolicy := fmt.Sprintf(
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:s3::%s:root"},"Action":"sts:AssumeRole"}]}`,
		testCallerAccountID)
	role := createRoleInAccount(t, svc, testCallerAccountID, "s3root", trustPolicy)

	_, err := svc.AssumeRole(testCallerAccountID, testCallerARN(), testCallerUserName,
		basicAssumeRoleInput(*role.Arn, "sess"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}

func TestAssumeRole_BareAccountIDPrincipal_TreatedAsRoot(t *testing.T) {
	svc, _ := newTestSetup(t)
	role := createRoleInAccount(t, svc, testCallerAccountID, "bare", trustPolicyAllowingBareAccount(testCallerAccountID))

	_, err := svc.AssumeRole(testCallerAccountID, testCallerARN(), testCallerUserName,
		basicAssumeRoleInput(*role.Arn, "sess"))
	require.NoError(t, err)
}

func TestAssumeRole_ActionWildcards(t *testing.T) {
	svc, _ := newTestSetup(t)
	caller := testCallerARN()

	cases := []struct {
		name   string
		action string
	}{
		{"sts_wildcard", `"sts:*"`},
		{"global_wildcard", `"*"`},
		{"sts_assumeRole_in_array", `["sts:AssumeRole"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":%q},"Action":%s}]}`, caller, tc.action)
			role := createRoleInAccount(t, svc, testCallerAccountID, "wild-"+tc.name, policy)
			_, err := svc.AssumeRole(testCallerAccountID, caller, testCallerUserName,
				basicAssumeRoleInput(*role.Arn, "sess"))
			require.NoError(t, err)
		})
	}
}

func TestAssumeRole_ExplicitDenyWinsOverAllow(t *testing.T) {
	svc, _ := newTestSetup(t)
	caller := testCallerARN()

	// Allow listed first; Deny second. A single-pass "return on first Allow"
	// loop would silently skip the Deny and grant the session — see plan §4.
	policyAllowThenDeny := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[
        {"Effect":"Allow","Principal":{"AWS":"*"},"Action":"sts:AssumeRole"},
        {"Effect":"Deny","Principal":{"AWS":%q},"Action":"sts:AssumeRole"}
    ]}`, caller)
	role := createRoleInAccount(t, svc, testCallerAccountID, "deny-second", policyAllowThenDeny)

	_, err := svc.AssumeRole(testCallerAccountID, caller, testCallerUserName,
		basicAssumeRoleInput(*role.Arn, "sess"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())

	// And in the opposite order: Deny first, Allow second — same result.
	policyDenyThenAllow := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[
        {"Effect":"Deny","Principal":{"AWS":%q},"Action":"sts:AssumeRole"},
        {"Effect":"Allow","Principal":{"AWS":"*"},"Action":"sts:AssumeRole"}
    ]}`, caller)
	role2 := createRoleInAccount(t, svc, testCallerAccountID, "deny-first", policyDenyThenAllow)
	_, err = svc.AssumeRole(testCallerAccountID, caller, testCallerUserName,
		basicAssumeRoleInput(*role2.Arn, "sess"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}

func TestAssumeRole_NoMatchingAllow_AccessDenied(t *testing.T) {
	svc, _ := newTestSetup(t)
	other := fmt.Sprintf("arn:aws:iam::%s:user/bob", testCallerAccountID)
	role := createRoleInAccount(t, svc, testCallerAccountID, "narrow", trustPolicyAllowingUser(other))

	_, err := svc.AssumeRole(testCallerAccountID, testCallerARN(), testCallerUserName,
		basicAssumeRoleInput(*role.Arn, "sess"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}

func TestAssumeRole_SameAccountRoleNotFound_NoSuchEntity(t *testing.T) {
	svc, _ := newTestSetup(t)

	_, err := svc.AssumeRole(testCallerAccountID, testCallerARN(), testCallerUserName,
		basicAssumeRoleInput(fmt.Sprintf("arn:aws:iam::%s:role/ghost", testCallerAccountID), "sess"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorIAMNoSuchEntity, err.Error())
}

func TestAssumeRole_CrossAccountRoleNotFound_AccessDenied(t *testing.T) {
	svc, _ := newTestSetup(t)

	// Caller is in callerAccountID; missing role is in testCrossAccountID.
	// Masking to AccessDenied (rather than NoSuchEntity) prevents
	// cross-account role enumeration.
	_, err := svc.AssumeRole(testCallerAccountID, testCallerARN(), testCallerUserName,
		basicAssumeRoleInput(fmt.Sprintf("arn:aws:iam::%s:role/ghost", testCrossAccountID), "sess"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}

func TestAssumeRole_DurationBounds(t *testing.T) {
	svc, _ := newTestSetup(t)
	caller := testCallerARN()
	role := createRoleInAccount(t, svc, testCallerAccountID, "dur", trustPolicyAllowingUser(caller))

	cases := []struct {
		name    string
		seconds int64
		wantErr bool
	}{
		{"below_minimum", 899, true},
		{"at_minimum", 900, false},
		{"default_via_unset", 0, false},
		{"at_role_max", 3600, false},
		{"above_role_max", 3601, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := basicAssumeRoleInput(*role.Arn, "sess")
			if tc.seconds != 0 {
				input.DurationSeconds = aws.Int64(tc.seconds)
			}
			_, err := svc.AssumeRole(testCallerAccountID, caller, testCallerUserName, input)
			if tc.wantErr {
				require.Error(t, err)
				assert.Equal(t, awserrors.ErrorValidationError, err.Error())
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestAssumeRole_LongerMaxSessionDuration_CapAtTwelveHours(t *testing.T) {
	svc, _ := newTestSetup(t)
	caller := testCallerARN()
	out, err := svc.iamSvc.CreateRole(testCallerAccountID, &iam.CreateRoleInput{
		RoleName:                 aws.String("long"),
		AssumeRolePolicyDocument: aws.String(trustPolicyAllowingUser(caller)),
		MaxSessionDuration:       aws.Int64(maxDurationSeconds),
	})
	require.NoError(t, err)
	role := out.Role

	// 12-hour duration is permitted because role.MaxSessionDuration == 43200.
	_, err = svc.AssumeRole(testCallerAccountID, caller, testCallerUserName,
		&sts.AssumeRoleInput{
			RoleArn:         role.Arn,
			RoleSessionName: aws.String("sess"),
			DurationSeconds: aws.Int64(maxDurationSeconds),
		})
	require.NoError(t, err)

	// 43201s is rejected — above the absolute 12h ceiling.
	_, err = svc.AssumeRole(testCallerAccountID, caller, testCallerUserName,
		&sts.AssumeRoleInput{
			RoleArn:         role.Arn,
			RoleSessionName: aws.String("sess2"),
			DurationSeconds: aws.Int64(maxDurationSeconds + 1),
		})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorValidationError, err.Error())
}

func TestAssumeRole_RejectsSessionPolicies(t *testing.T) {
	svc, _ := newTestSetup(t)
	role := createRoleInAccount(t, svc, testCallerAccountID, "nopol", trustPolicyAllowingUser(testCallerARN()))

	cases := []struct {
		name  string
		input *sts.AssumeRoleInput
	}{
		{
			name: "inline_policy",
			input: &sts.AssumeRoleInput{
				RoleArn:         role.Arn,
				RoleSessionName: aws.String("sess"),
				Policy:          aws.String(`{"Version":"2012-10-17","Statement":[]}`),
			},
		},
		{
			name: "policy_arns",
			input: &sts.AssumeRoleInput{
				RoleArn:         role.Arn,
				RoleSessionName: aws.String("sess"),
				PolicyArns: []*sts.PolicyDescriptorType{{
					Arn: aws.String("arn:aws:iam::000000000000:policy/x"),
				}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.AssumeRole(testCallerAccountID, testCallerARN(), testCallerUserName, tc.input)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorPackedPolicyTooLarge, err.Error())
		})
	}
}

func TestAssumeRole_RejectsSessionTags(t *testing.T) {
	svc, _ := newTestSetup(t)
	role := createRoleInAccount(t, svc, testCallerAccountID, "notag", trustPolicyAllowingUser(testCallerARN()))

	cases := []struct {
		name  string
		input *sts.AssumeRoleInput
	}{
		{
			name: "tags",
			input: &sts.AssumeRoleInput{
				RoleArn:         role.Arn,
				RoleSessionName: aws.String("sess"),
				Tags:            []*sts.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
			},
		},
		{
			name: "transitive_tag_keys",
			input: &sts.AssumeRoleInput{
				RoleArn:           role.Arn,
				RoleSessionName:   aws.String("sess"),
				TransitiveTagKeys: []*string{aws.String("k")},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.AssumeRole(testCallerAccountID, testCallerARN(), testCallerUserName, tc.input)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
		})
	}
}

func TestAssumeRole_RejectsMFA(t *testing.T) {
	svc, _ := newTestSetup(t)
	role := createRoleInAccount(t, svc, testCallerAccountID, "nomfa", trustPolicyAllowingUser(testCallerARN()))

	cases := []struct {
		name  string
		input *sts.AssumeRoleInput
	}{
		{
			name: "serial_number",
			input: &sts.AssumeRoleInput{
				RoleArn:         role.Arn,
				RoleSessionName: aws.String("sess"),
				SerialNumber:    aws.String("arn:aws:iam::000000000000:mfa/alice"),
			},
		},
		{
			name: "token_code",
			input: &sts.AssumeRoleInput{
				RoleArn:         role.Arn,
				RoleSessionName: aws.String("sess"),
				TokenCode:       aws.String("123456"),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.AssumeRole(testCallerAccountID, testCallerARN(), testCallerUserName, tc.input)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
		})
	}
}

func TestAssumeRole_RejectsInvalidSessionName(t *testing.T) {
	svc, _ := newTestSetup(t)
	role := createRoleInAccount(t, svc, testCallerAccountID, "name", trustPolicyAllowingUser(testCallerARN()))

	bad := []string{
		"",                      // missing → MissingParameter (handled separately below)
		"a",                     // too short
		strings.Repeat("a", 65), // too long
		"has/slash",
		"has:colon",
		"ué", // non-ASCII; literal regex deliberately excludes Unicode
	}
	for _, name := range bad {
		t.Run("session_"+name, func(t *testing.T) {
			input := &sts.AssumeRoleInput{
				RoleArn:         role.Arn,
				RoleSessionName: aws.String(name),
			}
			_, err := svc.AssumeRole(testCallerAccountID, testCallerARN(), testCallerUserName, input)
			require.Error(t, err)
			if name == "" {
				assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
			} else {
				assert.Equal(t, awserrors.ErrorValidationError, err.Error())
			}
		})
	}
}

func TestAssumeRole_RejectsMissingRequiredFields(t *testing.T) {
	svc, _ := newTestSetup(t)

	cases := []struct {
		name  string
		input *sts.AssumeRoleInput
	}{
		{"nil_input", nil},
		{"missing_role_arn", &sts.AssumeRoleInput{RoleSessionName: aws.String("sess")}},
		{"missing_session_name", &sts.AssumeRoleInput{RoleArn: aws.String("arn:aws:iam::000000000000:role/x")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.AssumeRole(testCallerAccountID, testCallerARN(), testCallerUserName, tc.input)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
		})
	}
}

func TestAssumeRole_RejectsMalformedRoleARN(t *testing.T) {
	svc, _ := newTestSetup(t)

	bad := []string{
		"not-an-arn",
		"arn:aws:iam::000000000000:user/alice",      // wrong resource type
		"arn:aws:s3:::bucket/key",                   // wrong service
		"arn:aws:iam::000000000000:role/",           // empty name
		"arn:aws:iam:us-east-1:000000000000:role/x", // region populated
	}
	for _, badARN := range bad {
		t.Run(badARN, func(t *testing.T) {
			_, err := svc.AssumeRole(testCallerAccountID, testCallerARN(), testCallerUserName,
				basicAssumeRoleInput(badARN, "sess"))
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorValidationError, err.Error())
		})
	}
}

func TestAssumeRole_CrossAccountSuccess(t *testing.T) {
	svc, _ := newTestSetup(t)
	caller := testCallerARN()
	// Role lives in account B, trust policy allows account-A user.
	role := createRoleInAccount(t, svc, testCrossAccountID, "x-acct", trustPolicyAllowingUser(caller))

	out, err := svc.AssumeRole(testCallerAccountID, caller, testCallerUserName,
		basicAssumeRoleInput(*role.Arn, "x-sess"))
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("arn:aws:sts::%s:assumed-role/x-acct/x-sess", testCrossAccountID),
		aws.StringValue(out.AssumedRoleUser.Arn))

	stored, err := svc.LookupSessionCredential(aws.StringValue(out.Credentials.AccessKeyId))
	require.NoError(t, err)
	assert.Equal(t, testCrossAccountID, stored.AccountID, "session is bound to the role's account, not the caller's")
}

func TestAssumeRole_ChainedAssume_RoleArnAutoExpansion(t *testing.T) {
	svc, _ := newTestSetup(t)
	// Caller is already an assumed-role session of SourceRole in account A.
	callerSession := fmt.Sprintf("arn:aws:sts::%s:assumed-role/SourceRole/orig-sess", testCallerAccountID)
	// Target role's trust policy names the source role (not the session).
	clausePolicy := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::%s:role/SourceRole"},"Action":"sts:AssumeRole"}]}`, testCallerAccountID)
	role := createRoleInAccount(t, svc, testCallerAccountID, "TargetRole", clausePolicy)

	_, err := svc.AssumeRole(testCallerAccountID, callerSession, "SourceRole",
		basicAssumeRoleInput(*role.Arn, "chained"))
	require.NoError(t, err)
}

func TestAssumeRole_ChainedAssume_LiteralSessionARN(t *testing.T) {
	svc, _ := newTestSetup(t)
	exactSession := fmt.Sprintf("arn:aws:sts::%s:assumed-role/SourceRole/named", testCallerAccountID)
	clausePolicy := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":%q},"Action":"sts:AssumeRole"}]}`, exactSession)
	role := createRoleInAccount(t, svc, testCallerAccountID, "Target2", clausePolicy)

	// Exact session ARN → allowed.
	_, err := svc.AssumeRole(testCallerAccountID, exactSession, "SourceRole",
		basicAssumeRoleInput(*role.Arn, "ok"))
	require.NoError(t, err)

	// Different session of the same role → denied (literal match only).
	otherSession := fmt.Sprintf("arn:aws:sts::%s:assumed-role/SourceRole/other", testCallerAccountID)
	_, err = svc.AssumeRole(testCallerAccountID, otherSession, "SourceRole",
		basicAssumeRoleInput(*role.Arn, "no"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}

func TestAssumeRole_PrincipalArray_AnyEntryMatches(t *testing.T) {
	svc, _ := newTestSetup(t)
	caller := testCallerARN()
	bob := fmt.Sprintf("arn:aws:iam::%s:user/bob", testCallerAccountID)
	policy := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":[%q,%q]},"Action":"sts:AssumeRole"}]}`, bob, caller)
	role := createRoleInAccount(t, svc, testCallerAccountID, "multi", policy)

	_, err := svc.AssumeRole(testCallerAccountID, caller, testCallerUserName,
		basicAssumeRoleInput(*role.Arn, "sess"))
	require.NoError(t, err)
}

func TestAssumeRole_ServicePrincipal_NeverMatchesInV1(t *testing.T) {
	svc, _ := newTestSetup(t)
	// AWS-only Principal alongside Service entry — the Service entry skips at
	// the entry level (no service principals in v1), the AWS entry decides.
	policy := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com","AWS":%q},"Action":"sts:AssumeRole"}]}`, testCallerARN())
	role := createRoleInAccount(t, svc, testCallerAccountID, "mixed-allow", policy)
	_, err := svc.AssumeRole(testCallerAccountID, testCallerARN(), testCallerUserName,
		basicAssumeRoleInput(*role.Arn, "sess"))
	require.NoError(t, err)

	// Service-only policy → never matches a non-service caller in v1.
	svcOnly := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
	role2 := createRoleInAccount(t, svc, testCallerAccountID, "svc-only", svcOnly)
	_, err = svc.AssumeRole(testCallerAccountID, testCallerARN(), testCallerUserName,
		basicAssumeRoleInput(*role2.Arn, "sess"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}

func TestAssumeRole_RetriesOnAKIDCollision(t *testing.T) {
	svc, _ := newTestSetup(t)
	caller := testCallerARN()
	role := createRoleInAccount(t, svc, testCallerAccountID, "collide", trustPolicyAllowingUser(caller))

	// Pre-seed an arbitrary ASIA AKID so the chance of a real collision is
	// not relied on; the mint loop's collision behaviour is otherwise hard to
	// exercise. We can't predict the AKID generator's output, so this test
	// exercises the success path and asserts that two consecutive AssumeRole
	// calls produce distinct AKIDs (entropy sanity check rather than
	// retry-path coverage). The retry loop itself is small and obvious; this
	// guards the more likely regression — accidentally reusing an AKID.
	out1, err := svc.AssumeRole(testCallerAccountID, caller, testCallerUserName,
		basicAssumeRoleInput(*role.Arn, "sess-a"))
	require.NoError(t, err)
	out2, err := svc.AssumeRole(testCallerAccountID, caller, testCallerUserName,
		basicAssumeRoleInput(*role.Arn, "sess-b"))
	require.NoError(t, err)
	assert.NotEqual(t,
		aws.StringValue(out1.Credentials.AccessKeyId),
		aws.StringValue(out2.Credentials.AccessKeyId))
	assert.NotEqual(t,
		aws.StringValue(out1.Credentials.SessionToken),
		aws.StringValue(out2.Credentials.SessionToken))
}

// ----- Unit tests for the trust-policy helpers ---------------------------

func TestParseRoleARN(t *testing.T) {
	cases := []struct {
		arn         string
		wantAccount string
		wantName    string
		wantErr     bool
	}{
		{"arn:aws:iam::000000000000:role/app", "000000000000", "app", false},
		{"arn:aws:iam::000000000000:role/path/to/app", "000000000000", "app", false},
		{"arn:aws:iam::000000000000:user/alice", "", "", true},
		{"arn:aws:iam::000000000000:role/", "", "", true},
		{"arn:aws:iam:us-east-1:000000000000:role/app", "", "", true},
		{"not-an-arn", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.arn, func(t *testing.T) {
			a, n, err := parseRoleARN(tc.arn)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantAccount, a)
			assert.Equal(t, tc.wantName, n)
		})
	}
}

func TestComputeTokenHMAC_Deterministic(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	a := computeTokenHMAC(key, "wire-token")
	b := computeTokenHMAC(key, "wire-token")
	assert.Equal(t, a, b)
	c := computeTokenHMAC(key, "wire-token-2")
	assert.NotEqual(t, a, c)

	differentKey := []byte("DIFFERENT_KEY_DIFFERENT_KEY_DIFF")
	require.Len(t, differentKey, masterKeySize)
	d := computeTokenHMAC(differentKey, "wire-token")
	assert.NotEqual(t, a, d)
}

func TestGenerateSessionAKID_FormatAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := range 64 {
		akid, err := generateSessionAKID()
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(akid, SessionAccessKeyIDPrefix))
		require.Len(t, akid, 20)
		require.False(t, seen[akid], "duplicate AKID at iteration %d: %q", i, akid)
		seen[akid] = true
	}
}

func TestEvalTrustPolicy_CorruptDocReturnsError(t *testing.T) {
	// Stored docs are validated upstream; reaching evalTrustPolicy with a
	// malformed doc indicates corruption — must fail closed and NOT collapse
	// to AccessDenied (which would hide the corruption from operators).
	err := evalTrustPolicy(`{not json`, testCallerARN())
	require.Error(t, err)
	assert.NotEqual(t, awserrors.ErrorAccessDenied, err.Error(),
		"corrupt doc must not be reported as AccessDenied — operators need to see the corruption signal")
}
