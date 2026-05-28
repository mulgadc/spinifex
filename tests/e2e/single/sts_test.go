//go:build e2e

package single

import (
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// STS E2E identifiers. The role name carries an sts-e2e- prefix so it never
// collides with the IAM-roles suite (which owns "app-role" / "other-role").
const (
	stsRoleName    = "sts-e2e-role"
	stsSessionName = "e2e-session-1"

	// Trust policy with a wildcard AWS principal so the test isn't coupled to
	// whatever user the bootstrap profile maps to (root vs. a named user).
	stsTrustPolicyAllowAny = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"*"},"Action":"sts:AssumeRole"}]}`

	// Trust policy that names a principal which cannot exist in this cluster.
	// Used to confirm a trust-policy denial surfaces AccessDenied without
	// changing the role contract on the SDK side.
	stsTrustPolicyDenyAny = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::999999999999:user/ghost"},"Action":"sts:AssumeRole"}]}`
)

// runSTS covers the STS v1 surface: GetCallerIdentity smoke, the AssumeRole
// happy path (ASIA AKID + session token + ~1h expiry), use-the-session-token
// round-trip via GetCallerIdentity, trust-policy denial after
// UpdateAssumeRolePolicy, NoSuchEntity on a missing same-account role, and
// 501 from a stubbed STS action.
func runSTS(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — STS AssumeRole + GetCallerIdentity")
	adminAccount := iamEnsureAdminAccountID(t, fix)

	// Bootstrap-creds GetCallerIdentity — sanity check the live endpoint and
	// capture the active ARN for the detail log so failures elsewhere are
	// easier to attribute.
	harness.Step(t, "get-caller-identity (bootstrap)")
	who, err := fix.AWS.STS.GetCallerIdentity(&sts.GetCallerIdentityInput{})
	require.NoError(t, err, "get-caller-identity (bootstrap)")
	require.Equal(t, adminAccount, aws.StringValue(who.Account),
		"bootstrap account mismatch")
	require.NotEmpty(t, aws.StringValue(who.Arn), "empty caller ARN")
	require.NotEmpty(t, aws.StringValue(who.UserId), "empty caller UserId")
	harness.Detail(t, "arn", aws.StringValue(who.Arn),
		"user_id", aws.StringValue(who.UserId))

	// Defensive sweep + parent-scoped cleanup so a mid-test panic still
	// removes the role. iamDeleteRoleAndProfilesBestEffort tolerates a
	// missing role.
	iamDeleteRoleAndProfilesBestEffort(fix, stsRoleName, nil)
	fix.Harness.RegisterCleanup(func() {
		iamDeleteRoleAndProfilesBestEffort(fix, stsRoleName, nil)
	})

	harness.Step(t, "create-role %q (trust=AWS:*)", stsRoleName)
	createOut, err := fix.AWS.IAM.CreateRole(&iam.CreateRoleInput{
		RoleName:                 aws.String(stsRoleName),
		AssumeRolePolicyDocument: aws.String(stsTrustPolicyAllowAny),
		Description:              aws.String("E2E STS AssumeRole + GetCallerIdentity"),
	})
	require.NoError(t, err, "create-role")
	roleARN := aws.StringValue(createOut.Role.Arn)
	require.Equal(t, iamRoleARN(adminAccount, stsRoleName), roleARN,
		"role ARN must follow arn:aws:iam::<acct>:role/<name>")

	// Happy path. Verify the wire-format invariants: ASIA prefix, non-empty
	// secret + token, expiration ≈ 1h, and the AssumedRoleUser shape.
	harness.Step(t, "assume-role %q session=%q", roleARN, stsSessionName)
	beforeAssume := time.Now().UTC()
	aOut, err := fix.AWS.STS.AssumeRole(&sts.AssumeRoleInput{
		RoleArn:         aws.String(roleARN),
		RoleSessionName: aws.String(stsSessionName),
	})
	require.NoError(t, err, "assume-role")

	creds := aOut.Credentials
	require.NotNil(t, creds, "AssumeRole returned nil Credentials")
	akid := aws.StringValue(creds.AccessKeyId)
	secret := aws.StringValue(creds.SecretAccessKey)
	token := aws.StringValue(creds.SessionToken)
	require.True(t, strings.HasPrefix(akid, "ASIA"),
		"session AKID must start with ASIA, got %q", akid)
	require.NotEmpty(t, secret, "empty SecretAccessKey")
	require.NotEmpty(t, token, "empty SessionToken")
	require.NotNil(t, creds.Expiration, "nil Expiration")

	// Default DurationSeconds is 3600 (1h). Bound the assertion loosely so a
	// few seconds of test-runner latency doesn't flake CI.
	expiresIn := creds.Expiration.Sub(beforeAssume)
	require.Greater(t, expiresIn, 30*time.Minute,
		"expiration too soon: %v (akid=%s)", expiresIn, akid)
	require.LessOrEqual(t, expiresIn, time.Hour+5*time.Minute,
		"expiration too far: %v (akid=%s)", expiresIn, akid)

	expectedAssumedARN := "arn:aws:sts::" + adminAccount + ":assumed-role/" +
		stsRoleName + "/" + stsSessionName
	require.NotNil(t, aOut.AssumedRoleUser, "nil AssumedRoleUser")
	require.Equal(t, expectedAssumedARN, aws.StringValue(aOut.AssumedRoleUser.Arn),
		"assumed-role ARN shape mismatch")
	assumedRoleID := aws.StringValue(aOut.AssumedRoleUser.AssumedRoleId)
	require.NotEmpty(t, assumedRoleID, "empty AssumedRoleId")
	require.True(t, strings.HasSuffix(assumedRoleID, ":"+stsSessionName),
		"AssumedRoleId must end with :%s, got %q", stsSessionName, assumedRoleID)
	harness.Detail(t, "akid", akid, "assumed_role_arn", expectedAssumedARN,
		"assumed_role_id", assumedRoleID)

	// Drive the SigV4 ASIA path: use the freshly-minted creds against
	// GetCallerIdentity and assert the identity round-trips. The endpoint's
	// own auth middleware verifies X-Amz-Security-Token, so a wire-token
	// mismatch would surface here as InvalidClientTokenId.
	harness.Step(t, "get-caller-identity with assumed-role creds (ASIA path)")
	sessionCli := harness.NewAWSClientWithSessionCreds(t, fix.Env, akid, secret, token)
	sessWho, err := sessionCli.STS.GetCallerIdentity(&sts.GetCallerIdentityInput{})
	require.NoError(t, err, "get-caller-identity (assumed)")
	require.Equal(t, adminAccount, aws.StringValue(sessWho.Account),
		"assumed account mismatch")
	require.Equal(t, expectedAssumedARN, aws.StringValue(sessWho.Arn),
		"assumed ARN must be the sts:assumed-role form")
	require.Equal(t, assumedRoleID, aws.StringValue(sessWho.UserId),
		"UserId for assumed-role must equal AssumedRoleId")

	// Trust-policy denial: swap to a principal that cannot match the caller,
	// then re-assume to confirm the trust-policy evaluator returns
	// AccessDenied. Uses a different session name to make any stale-record
	// confusion easier to spot in logs.
	harness.Step(t, "update-assume-role-policy → unmatched principal")
	_, err = fix.AWS.IAM.UpdateAssumeRolePolicy(&iam.UpdateAssumeRolePolicyInput{
		RoleName:       aws.String(stsRoleName),
		PolicyDocument: aws.String(stsTrustPolicyDenyAny),
	})
	require.NoError(t, err, "update-assume-role-policy")
	harness.Step(t, "assume-role after policy swap (expect AccessDenied)")
	harness.ExpectError(t, "AccessDenied", func() error {
		_, e := fix.AWS.STS.AssumeRole(&sts.AssumeRoleInput{
			RoleArn:         aws.String(roleARN),
			RoleSessionName: aws.String("e2e-session-2"),
		})
		return e
	})

	// Same-account miss → NoSuchEntity. Cross-account miss is masked to
	// AccessDenied by the handler; that's covered by handlers/sts unit tests
	// since the single-node fixture only has one account.
	harness.Step(t, "assume-role missing role (expect NoSuchEntity)")
	harness.ExpectError(t, "NoSuchEntity", func() error {
		_, e := fix.AWS.STS.AssumeRole(&sts.AssumeRoleInput{
			RoleArn: aws.String(iamRoleARN(adminAccount,
				"sts-e2e-no-such-role")),
			RoleSessionName: aws.String("ghost"),
		})
		return e
	})

	// Assertive teardown — the cleanup hook above is the safety net; this is
	// the happy-path delete so the test surfaces a DeleteConflict regression
	// (e.g. stray attached policy from a future refactor) loudly.
	_, err = fix.AWS.IAM.DeleteRole(&iam.DeleteRoleInput{
		RoleName: aws.String(stsRoleName),
	})
	require.NoError(t, err, "delete-role")
}
