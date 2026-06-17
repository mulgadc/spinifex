//go:build e2e

package iam

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
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

	// Chained-assume target: a second role whose trust policy names the
	// first role's IAM ARN. Exercises the role-ARN-clause vs. session-ARN-
	// caller auto-expansion path in the trust-policy matcher.
	stsRoleNameChain    = "sts-e2e-role-chain"
	stsSessionNameChain = "e2e-chain-session"

	// Role names used only by the write-time-rejection sub-tests. CreateRole
	// is expected to fail before any persistence, so cleanup is best-effort.
	stsRoleNameForbidCondition    = "sts-e2e-forbid-condition"
	stsRoleNameForbidNotPrincipal = "sts-e2e-forbid-notprincipal"
	stsRoleNameForbidNotAction    = "sts-e2e-forbid-notaction"

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
// round-trip via GetCallerIdentity, the cross-service assumed-role denial at
// the gateway policy gate, chained assume (assumed-role session calls
// AssumeRole on a second role whose trust policy names the source role),
// trust-policy denial after UpdateAssumeRolePolicy, NoSuchEntity on a missing
// same-account role, and write-time rejection of forbidden trust-policy
// blocks (Condition / NotPrincipal / NotAction) via the SDK.
func runSTS(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — STS AssumeRole + GetCallerIdentity")
	adminAccount := harness.IAMAccountID(t, fix.AWS)

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

	// GetSessionToken: exchange the bootstrap user's long-lived (AKIA) creds for
	// short-lived ASIA session creds bound to the SAME identity. Unlike
	// AssumeRole, the session must resolve back to the original caller ARN, not
	// an assumed-role ARN. This is the behaviour unit tests cannot fully prove —
	// real HMAC token verification + wire round-trip + the user-principal branch
	// in resolveSessionAKID.
	harness.Step(t, "get-session-token (user creds → ASIA)")
	beforeGST := time.Now().UTC()
	gstOut, err := fix.AWS.STS.GetSessionToken(&sts.GetSessionTokenInput{})
	require.NoError(t, err, "get-session-token")
	gstCreds := gstOut.Credentials
	require.NotNil(t, gstCreds, "GetSessionToken returned nil Credentials")
	gstAKID := aws.StringValue(gstCreds.AccessKeyId)
	gstSecret := aws.StringValue(gstCreds.SecretAccessKey)
	gstToken := aws.StringValue(gstCreds.SessionToken)
	require.True(t, strings.HasPrefix(gstAKID, "ASIA"),
		"session AKID must start with ASIA, got %q", gstAKID)
	require.NotEmpty(t, gstSecret, "empty SecretAccessKey")
	require.NotEmpty(t, gstToken, "empty SessionToken")
	require.NotNil(t, gstCreds.Expiration, "nil Expiration")

	// Default DurationSeconds is 43200 (12h). Bound loosely so runner latency
	// doesn't flake CI.
	gstExpiresIn := gstCreds.Expiration.Sub(beforeGST)
	require.Greater(t, gstExpiresIn, 11*time.Hour,
		"get-session-token expiration too soon: %v (akid=%s)", gstExpiresIn, gstAKID)
	require.LessOrEqual(t, gstExpiresIn, 12*time.Hour+5*time.Minute,
		"get-session-token expiration too far: %v (akid=%s)", gstExpiresIn, gstAKID)
	harness.Detail(t, "gst_akid", gstAKID, "gst_expires_in", gstExpiresIn.String())

	// Drive the ASIA SigV4 path with the session creds: GetCallerIdentity must
	// return the ORIGINAL caller identity (same Account/Arn/UserId as the
	// bootstrap user), proving the user-principal branch resolves back to the
	// user rather than synthesising an assumed-role ARN.
	harness.Step(t, "get-caller-identity with get-session-token creds (expect same user)")
	gstCli := harness.NewAWSClientWithSessionCreds(t, fix.Env, gstAKID, gstSecret, gstToken)
	gstWho, err := gstCli.STS.GetCallerIdentity(&sts.GetCallerIdentityInput{})
	require.NoError(t, err, "get-caller-identity (get-session-token)")
	require.Equal(t, aws.StringValue(who.Account), aws.StringValue(gstWho.Account),
		"get-session-token session account must match the calling user")
	require.Equal(t, aws.StringValue(who.Arn), aws.StringValue(gstWho.Arn),
		"get-session-token session must resolve to the user ARN, not an assumed-role ARN")
	require.Equal(t, aws.StringValue(who.UserId), aws.StringValue(gstWho.UserId),
		"get-session-token session UserId must match the calling user")

	// The user session must be authorised AS THE USER for non-STS actions:
	// drive a real EC2 endpoint with the GetSessionToken creds and assert it is
	// NOT denied. This is the only wire-level proof that gateway.checkPolicy
	// evaluates policy against the user principal — the assumed-role path below
	// hard-denies, so a unit test cannot exercise ctxPrincipalType=user reaching
	// the EC2 dispatcher.
	harness.Step(t, "ec2 describe-instances with get-session-token creds (expect authorised)")
	_, err = gstCli.EC2.DescribeInstances(&ec2.DescribeInstancesInput{})
	require.NoError(t, err, "user session must be authorised for ec2:DescribeInstances as the user")

	// GetSessionToken is long-lived-user-only: a GetSessionToken session
	// resolves back to principalType "user", so its ASIA access-key prefix is
	// the only signal that it is a temporary credential. Replaying it into
	// GetSessionToken must be denied — otherwise a captured session could roll
	// its own lifetime forward forever. The wire path proves the handler's
	// ASIA-prefix guard sees c.accessKey from the SigV4 context, which a unit
	// test of the handler cannot exercise.
	harness.Step(t, "get-session-token with get-session-token creds (expect AccessDenied)")
	harness.ExpectError(t, "AccessDenied", func() error {
		_, e := gstCli.STS.GetSessionToken(&sts.GetSessionTokenInput{})
		return e
	})

	// Defensive sweep + parent-scoped cleanup so a mid-test panic still
	// removes the role. iamDeleteRoleAndProfilesBestEffort tolerates a
	// missing role. Cleans up every role the suite might create — the
	// forbid-* roles are expected to fail at CreateRole but the sweep is
	// cheap and protects against a future validator regression that lets
	// one slip through.
	stsRoles := []string{
		stsRoleName,
		stsRoleNameChain,
		stsRoleNameForbidCondition,
		stsRoleNameForbidNotPrincipal,
		stsRoleNameForbidNotAction,
	}
	for _, name := range stsRoles {
		harness.IAMDeleteRoleAndProfilesBestEffort(fix.AWS, name, nil)
	}
	fix.Harness.RegisterCleanup(func() {
		for _, name := range stsRoles {
			harness.IAMDeleteRoleAndProfilesBestEffort(fix.AWS, name, nil)
		}
	})

	harness.Step(t, "create-role %q (trust=AWS:*)", stsRoleName)
	createOut, err := fix.AWS.IAM.CreateRole(&iam.CreateRoleInput{
		RoleName:                 aws.String(stsRoleName),
		AssumeRolePolicyDocument: aws.String(stsTrustPolicyAllowAny),
		Description:              aws.String("E2E STS AssumeRole + GetCallerIdentity"),
	})
	require.NoError(t, err, "create-role")
	roleARN := aws.StringValue(createOut.Role.Arn)
	require.Equal(t, harness.IAMRoleARN(adminAccount, stsRoleName), roleARN,
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

	// Cross-service ASIA SigV4: drive a non-STS endpoint with the assumed
	// creds. The role here carries no permission policy, so gateway.checkPolicy
	// resolves its (empty) managed policies and the action falls to an implicit
	// deny — assumability does not imply permissions. The wire path also proves
	// ctxPrincipalType + the underlying-role ARN propagate beyond the STS
	// service into the EC2 dispatcher's policy gate; a unit test of checkPolicy
	// cannot exercise that propagation. The granted-policy positive case lives
	// in TestAssumedRoleControlPlaneEnforcement.
	harness.Step(t, "ec2 describe-regions with assumed-role creds (expect AccessDenied)")
	harness.ExpectError(t, "AccessDenied", func() error {
		_, e := sessionCli.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
		return e
	})

	// GetSessionToken is user-only: an assumed-role (ASIA) session must NOT be
	// able to mint a user session. AWS forbids calling GetSessionToken with
	// temporary credentials; the handler enforces it via the resolved principal
	// type. The wire path proves ctxPrincipalType=assumed-role reaches the
	// handler's guard — a unit test of the handler cannot exercise that
	// propagation from the SigV4 ASIA verifier.
	harness.Step(t, "get-session-token with assumed-role creds (expect AccessDenied)")
	harness.ExpectError(t, "AccessDenied", func() error {
		_, e := sessionCli.STS.GetSessionToken(&sts.GetSessionTokenInput{})
		return e
	})

	// --- Edge cases: tampered token, rejected params, duration clamp ---------
	// Run while sts-e2e-role still trusts AWS:* and carries no
	// MaxSessionDuration, so the duration ceiling is the 3600s default.

	// Tampered session token → InvalidClientTokenId. Reuse the happy-path ASIA
	// akid+secret but present a forged X-Amz-Security-Token. resolveSessionAKID
	// verifies the token HMAC before the request signature, so the mismatch
	// surfaces as InvalidClientTokenId regardless of the (valid) SigV4 sig. The
	// probe is an EC2 call: the gateway serialises SigV4 auth failures in the EC2
	// XML envelope (writeSigV4Error), which the STS Query client can't unmarshal
	// (it masks the code as SerializationError). The HMAC branch is
	// service-agnostic, so EC2 drives the same gateway/auth.go path with a
	// response the SDK decodes into the asserted code.
	harness.Step(t, "ec2 describe-regions with tampered session token (expect InvalidClientTokenId)")
	tamperedCli := harness.NewAWSClientWithSessionCreds(t, fix.Env, akid, secret, token+"-tampered")
	harness.ExpectError(t, "InvalidClientTokenId", func() error {
		_, e := tamperedCli.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
		return e
	})

	// Rejected-parameter wire rejections. The handler refuses inline session
	// policies, session tags, and MFA up front; each sub-test sets only its own
	// field so the field-specific code is unambiguous. Proves the SDK marshals
	// these over the wire and the gateway returns the AWS code rather than
	// silently dropping them.
	harness.Step(t, "assume-role with inline Policy (expect PackedPolicyTooLarge)")
	harness.ExpectError(t, "PackedPolicyTooLarge", func() error {
		_, e := fix.AWS.STS.AssumeRole(&sts.AssumeRoleInput{
			RoleArn:         aws.String(roleARN),
			RoleSessionName: aws.String("e2e-reject-policy"),
			Policy:          aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`),
		})
		return e
	})
	harness.Step(t, "assume-role with session Tags (expect InvalidParameterValue)")
	harness.ExpectError(t, "InvalidParameterValue", func() error {
		_, e := fix.AWS.STS.AssumeRole(&sts.AssumeRoleInput{
			RoleArn:         aws.String(roleARN),
			RoleSessionName: aws.String("e2e-reject-tags"),
			Tags:            []*sts.Tag{{Key: aws.String("team"), Value: aws.String("eng")}},
		})
		return e
	})
	harness.Step(t, "assume-role with MFA SerialNumber+TokenCode (expect InvalidParameterValue)")
	harness.ExpectError(t, "InvalidParameterValue", func() error {
		_, e := fix.AWS.STS.AssumeRole(&sts.AssumeRoleInput{
			RoleArn:         aws.String(roleARN),
			RoleSessionName: aws.String("e2e-reject-mfa"),
			SerialNumber:    aws.String("arn:aws:iam::" + adminAccount + ":mfa/e2e"),
			TokenCode:       aws.String("123456"),
		})
		return e
	})

	// DurationSeconds × role MaxSessionDuration clamp. With no MaxSessionDuration
	// on the role the ceiling is the 3600s default: 900s mints a ~15m session and
	// 7200s is rejected. After raising MaxSessionDuration to 7200, 7200s mints a
	// ~2h session and 10800s is rejected.
	harness.Step(t, "assume-role DurationSeconds=900 (expect ~15m expiry)")
	beforeShort := time.Now().UTC()
	shortOut, err := fix.AWS.STS.AssumeRole(&sts.AssumeRoleInput{
		RoleArn:         aws.String(roleARN),
		RoleSessionName: aws.String("e2e-dur-900"),
		DurationSeconds: aws.Int64(900),
	})
	require.NoError(t, err, "assume-role DurationSeconds=900")
	require.NotNil(t, shortOut.Credentials, "nil Credentials for DurationSeconds=900")
	shortExpiresIn := shortOut.Credentials.Expiration.Sub(beforeShort)
	require.Greater(t, shortExpiresIn, 14*time.Minute,
		"DurationSeconds=900 expiry too soon: %v", shortExpiresIn)
	require.LessOrEqual(t, shortExpiresIn, 16*time.Minute,
		"DurationSeconds=900 expiry too far: %v", shortExpiresIn)

	harness.Step(t, "assume-role DurationSeconds=7200 over default 3600 ceiling (expect ValidationError)")
	harness.ExpectError(t, "ValidationError", func() error {
		_, e := fix.AWS.STS.AssumeRole(&sts.AssumeRoleInput{
			RoleArn:         aws.String(roleARN),
			RoleSessionName: aws.String("e2e-dur-7200-over"),
			DurationSeconds: aws.Int64(7200),
		})
		return e
	})

	harness.Step(t, "update-role %q MaxSessionDuration=7200", stsRoleName)
	_, err = fix.AWS.IAM.UpdateRole(&iam.UpdateRoleInput{
		RoleName:           aws.String(stsRoleName),
		MaxSessionDuration: aws.Int64(7200),
	})
	require.NoError(t, err, "update-role MaxSessionDuration=7200")

	harness.Step(t, "assume-role DurationSeconds=7200 under raised ceiling (expect ~2h expiry)")
	beforeLong := time.Now().UTC()
	longOut, err := fix.AWS.STS.AssumeRole(&sts.AssumeRoleInput{
		RoleArn:         aws.String(roleARN),
		RoleSessionName: aws.String("e2e-dur-7200-ok"),
		DurationSeconds: aws.Int64(7200),
	})
	require.NoError(t, err, "assume-role DurationSeconds=7200 after raise")
	require.NotNil(t, longOut.Credentials, "nil Credentials for DurationSeconds=7200")
	longExpiresIn := longOut.Credentials.Expiration.Sub(beforeLong)
	require.Greater(t, longExpiresIn, time.Hour+50*time.Minute,
		"DurationSeconds=7200 expiry too soon: %v", longExpiresIn)
	require.LessOrEqual(t, longExpiresIn, 2*time.Hour+5*time.Minute,
		"DurationSeconds=7200 expiry too far: %v", longExpiresIn)

	harness.Step(t, "assume-role DurationSeconds=10800 over raised 7200 ceiling (expect ValidationError)")
	harness.ExpectError(t, "ValidationError", func() error {
		_, e := fix.AWS.STS.AssumeRole(&sts.AssumeRoleInput{
			RoleArn:         aws.String(roleARN),
			RoleSessionName: aws.String("e2e-dur-10800-over"),
			DurationSeconds: aws.Int64(10800),
		})
		return e
	})

	// Chained assume: create a second role whose trust policy names the
	// first role's IAM ARN, then have the assumed-role session call
	// AssumeRole on it. Exercises the role-ARN-clause vs. session-ARN-
	// caller auto-expansion path end-to-end — eval logic is unit-tested,
	// the wire flow (ASIA SigV4 → ctxAssumedRoleARN → handler caller ARN
	// → trust-policy match → new ASIA mint) is not.
	chainedTrustPolicy := fmt.Sprintf(
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":%q},"Action":"sts:AssumeRole"}]}`,
		harness.IAMRoleARN(adminAccount, stsRoleName),
	)
	harness.Step(t, "create-role %q (trust=role/%s for chained assume)",
		stsRoleNameChain, stsRoleName)
	chainCreateOut, err := fix.AWS.IAM.CreateRole(&iam.CreateRoleInput{
		RoleName:                 aws.String(stsRoleNameChain),
		AssumeRolePolicyDocument: aws.String(chainedTrustPolicy),
		Description:              aws.String("E2E STS chained AssumeRole target"),
	})
	require.NoError(t, err, "create-role (chain)")
	chainedRoleARN := aws.StringValue(chainCreateOut.Role.Arn)

	harness.Step(t, "assume-role %q via assumed-role session (chained)", chainedRoleARN)
	chainOut, err := sessionCli.STS.AssumeRole(&sts.AssumeRoleInput{
		RoleArn:         aws.String(chainedRoleARN),
		RoleSessionName: aws.String(stsSessionNameChain),
	})
	require.NoError(t, err, "assume-role (chain)")
	require.NotNil(t, chainOut.Credentials, "chained AssumeRole returned nil Credentials")
	chainAKID := aws.StringValue(chainOut.Credentials.AccessKeyId)
	chainSecret := aws.StringValue(chainOut.Credentials.SecretAccessKey)
	chainToken := aws.StringValue(chainOut.Credentials.SessionToken)
	require.True(t, strings.HasPrefix(chainAKID, "ASIA"),
		"chained session AKID must start with ASIA, got %q", chainAKID)
	require.NotEmpty(t, chainSecret, "empty chained SecretAccessKey")
	require.NotEmpty(t, chainToken, "empty chained SessionToken")

	expectedChainARN := "arn:aws:sts::" + adminAccount + ":assumed-role/" +
		stsRoleNameChain + "/" + stsSessionNameChain
	require.NotNil(t, chainOut.AssumedRoleUser, "nil chained AssumedRoleUser")
	require.Equal(t, expectedChainARN, aws.StringValue(chainOut.AssumedRoleUser.Arn),
		"chained assumed-role ARN shape mismatch")
	harness.Detail(t, "chain_akid", chainAKID, "chain_arn", expectedChainARN)

	// Round-trip the chained creds to prove the new session token verifies
	// on the wire and the gateway reports the chained-role ARN, not the
	// source-role ARN. Catches a regression that reuses the source session
	// token or fails to refresh ctxAssumedRoleARN on the new session.
	harness.Step(t, "get-caller-identity with chained assumed-role creds")
	chainCli := harness.NewAWSClientWithSessionCreds(t, fix.Env, chainAKID, chainSecret, chainToken)
	chainWho, err := chainCli.STS.GetCallerIdentity(&sts.GetCallerIdentityInput{})
	require.NoError(t, err, "get-caller-identity (chained)")
	require.Equal(t, expectedChainARN, aws.StringValue(chainWho.Arn),
		"chained GetCallerIdentity must report the chained-role ARN, not the source-role ARN")

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
			RoleArn: aws.String(harness.IAMRoleARN(adminAccount,
				"sts-e2e-no-such-role")),
			RoleSessionName: aws.String("ghost"),
		})
		return e
	})

	// Write-time trust-policy rejection via the SDK: Condition, NotPrincipal,
	// and NotAction must be refused at CreateRole rather than silently
	// accepted (silent-allow vector). Handler-level unit tests cover the
	// validator; the e2e value here is proving the SDK marshals these fields
	// over the wire intact and the gateway returns the AWS-conformant
	// MalformedPolicyDocument code rather than a generic ValidationError.
	for _, tc := range []struct {
		label    string
		roleName string
		doc      string
	}{
		{
			label:    "Condition",
			roleName: stsRoleNameForbidCondition,
			doc:      `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"*"},"Action":"sts:AssumeRole","Condition":{"StringEquals":{"sts:ExternalId":"abc"}}}]}`,
		},
		{
			label:    "NotPrincipal",
			roleName: stsRoleNameForbidNotPrincipal,
			doc:      `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","NotPrincipal":{"AWS":"arn:aws:iam::123456789012:user/Bob"},"Action":"sts:AssumeRole"}]}`,
		},
		{
			label:    "NotAction",
			roleName: stsRoleNameForbidNotAction,
			doc:      `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"*"},"NotAction":["sts:AssumeRole"]}]}`,
		},
	} {
		harness.Step(t, "create-role with %s trust block (expect MalformedPolicyDocument)", tc.label)
		harness.ExpectError(t, "MalformedPolicyDocument", func() error {
			_, e := fix.AWS.IAM.CreateRole(&iam.CreateRoleInput{
				RoleName:                 aws.String(tc.roleName),
				AssumeRolePolicyDocument: aws.String(tc.doc),
			})
			return e
		})
	}

	// Assertive teardown — the cleanup hook above is the safety net; this is
	// the happy-path delete so the test surfaces a DeleteConflict regression
	// (e.g. stray attached policy from a future refactor) loudly. The chained
	// role tears down first so its trust-policy dependency on stsRoleName
	// doesn't matter, but DeleteRole is unaffected by trust-policy references
	// in either direction — order is purely for readability.
	_, err = fix.AWS.IAM.DeleteRole(&iam.DeleteRoleInput{
		RoleName: aws.String(stsRoleNameChain),
	})
	require.NoError(t, err, "delete-role (chain)")
	_, err = fix.AWS.IAM.DeleteRole(&iam.DeleteRoleInput{
		RoleName: aws.String(stsRoleName),
	})
	require.NoError(t, err, "delete-role")
}
