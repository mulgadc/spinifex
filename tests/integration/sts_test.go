//go:build integration

package integration

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/stretchr/testify/require"
)

// STS identifiers. The role name carries an sts- prefix so it never collides
// with the IAM-roles suite's "app-role" / "other-profile" names, even though
// each test now runs against its own gateway.
const (
	stsRoleName    = "sts-role"
	stsSessionName = "session-1"

	// Chained-assume target: a second role whose trust policy names the
	// first role's IAM ARN. Exercises the role-ARN-clause vs. session-ARN-
	// caller auto-expansion path in the trust-policy matcher.
	stsRoleNameChain    = "sts-role-chain"
	stsSessionNameChain = "chain-session"

	// Role names used only by the write-time-rejection sub-tests. CreateRole
	// is expected to fail before any persistence.
	stsRoleNameForbidCondition    = "sts-forbid-condition"
	stsRoleNameForbidNotPrincipal = "sts-forbid-notprincipal"
	stsRoleNameForbidNotAction    = "sts-forbid-notaction"

	// Trust policy with a wildcard AWS principal so the test isn't coupled to
	// whatever user the bootstrap profile maps to (root vs. a named user).
	stsTrustPolicyAllowAny = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"*"},"Action":"sts:AssumeRole"}]}`

	// Trust policy that names a principal which cannot exist in this cluster.
	// Used to confirm a trust-policy denial surfaces AccessDenied without
	// changing the role contract on the SDK side.
	stsTrustPolicyDenyAny = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::999999999999:user/ghost"},"Action":"sts:AssumeRole"}]}`
)

// TestSTSAssumeRoleAndGetCallerIdentity covers the STS v1 surface:
// GetCallerIdentity smoke, GetSessionToken (long-lived-user creds -> ASIA,
// still resolving to the user identity), the AssumeRole happy path (ASIA
// AKID + session token + ~1h expiry), cross-service assumed-role denial at
// the gateway policy gate, chained assume (assumed-role session calls
// AssumeRole on a second role whose trust policy names the source role),
// trust-policy denial after UpdateAssumeRolePolicy, NoSuchEntity-masked
// missing-role denial, write-time rejection of forbidden trust-policy blocks
// (Condition / NotPrincipal / NotAction), and the DurationSeconds x role
// MaxSessionDuration clamp. STS dispatches straight to gw.STSService/
// gw.IAMService with no NATS hop, except the two EC2 authorization probes
// below, which need the DescribeInstances daemon subjects stubbed.
func TestSTSAssumeRoleAndGetCallerIdentity(t *testing.T) {
	gw := StartGateway(t)
	iamCli := gw.IAMClient(t)
	stsCli := gw.STSClient(t)
	adminAccount := gw.AccountID

	stubEmptyInstanceBuckets(t, gw)
	gw.StubSubject(t, "ec2.DescribeInstances", mustMarshal(t, &ec2.DescribeInstancesOutput{}))

	// Bootstrap-creds GetCallerIdentity — sanity check the endpoint and
	// capture the active ARN.
	who, err := stsCli.GetCallerIdentity(&sts.GetCallerIdentityInput{})
	require.NoError(t, err, "get-caller-identity (bootstrap)")
	require.Equal(t, adminAccount, aws.StringValue(who.Account), "bootstrap account mismatch")
	require.NotEmpty(t, aws.StringValue(who.Arn), "empty caller ARN")
	require.NotEmpty(t, aws.StringValue(who.UserId), "empty caller UserId")

	// GetSessionToken: exchange the bootstrap user's long-lived (AKIA) creds
	// for short-lived ASIA session creds bound to the SAME identity. Unlike
	// AssumeRole, the session must resolve back to the original caller ARN,
	// not an assumed-role ARN.
	beforeGST := time.Now().UTC()
	gstOut, err := stsCli.GetSessionToken(&sts.GetSessionTokenInput{})
	require.NoError(t, err, "get-session-token")
	gstCreds := gstOut.Credentials
	require.NotNil(t, gstCreds, "GetSessionToken returned nil Credentials")
	gstAKID := aws.StringValue(gstCreds.AccessKeyId)
	gstSecret := aws.StringValue(gstCreds.SecretAccessKey)
	gstToken := aws.StringValue(gstCreds.SessionToken)
	require.True(t, strings.HasPrefix(gstAKID, "ASIA"), "session AKID must start with ASIA, got %q", gstAKID)
	require.NotEmpty(t, gstSecret, "empty SecretAccessKey")
	require.NotEmpty(t, gstToken, "empty SessionToken")
	require.NotNil(t, gstCreds.Expiration, "nil Expiration")

	// Default DurationSeconds is 43200 (12h). Bound loosely so runner latency
	// doesn't flake CI.
	gstExpiresIn := gstCreds.Expiration.Sub(beforeGST)
	require.Greater(t, gstExpiresIn, 11*time.Hour, "get-session-token expiration too soon: %v (akid=%s)", gstExpiresIn, gstAKID)
	require.LessOrEqual(t, gstExpiresIn, 12*time.Hour+5*time.Minute, "get-session-token expiration too far: %v (akid=%s)", gstExpiresIn, gstAKID)

	// Drive the ASIA SigV4 path with the session creds: GetCallerIdentity
	// must return the ORIGINAL caller identity, proving the user-principal
	// branch resolves back to the user rather than synthesising an
	// assumed-role ARN.
	gstCli := gw.ClientsWithSessionCreds(t, gstAKID, gstSecret, gstToken)
	gstWho, err := gstCli.STS.GetCallerIdentity(&sts.GetCallerIdentityInput{})
	require.NoError(t, err, "get-caller-identity (get-session-token)")
	require.Equal(t, aws.StringValue(who.Account), aws.StringValue(gstWho.Account), "get-session-token session account must match the calling user")
	require.Equal(t, aws.StringValue(who.Arn), aws.StringValue(gstWho.Arn), "get-session-token session must resolve to the user ARN, not an assumed-role ARN")
	require.Equal(t, aws.StringValue(who.UserId), aws.StringValue(gstWho.UserId), "get-session-token session UserId must match the calling user")

	// The user session must be authorised AS THE USER for non-STS actions.
	_, err = gstCli.EC2.DescribeInstances(&ec2.DescribeInstancesInput{})
	require.NoError(t, err, "user session must be authorised for ec2:DescribeInstances as the user")

	// GetSessionToken is long-lived-user-only: replaying an ASIA session into
	// GetSessionToken must be denied, otherwise a captured session could roll
	// its own lifetime forward forever.
	_, err = gstCli.STS.GetSessionToken(&sts.GetSessionTokenInput{})
	requireAWSErrorCode(t, err, "AccessDenied")

	// --- Edge cases: tampered token, rejected params, duration clamp ---------

	createOut, err := iamCli.CreateRole(&iam.CreateRoleInput{
		RoleName:                 aws.String(stsRoleName),
		AssumeRolePolicyDocument: aws.String(stsTrustPolicyAllowAny),
		Description:              aws.String("integration test STS AssumeRole + GetCallerIdentity"),
	})
	require.NoError(t, err, "create-role")
	roleARN := aws.StringValue(createOut.Role.Arn)
	require.Equal(t, iamRoleARN(adminAccount, stsRoleName), roleARN, "role ARN must follow arn:aws:iam::<acct>:role/<name>")

	// Happy path. Verify the wire-format invariants: ASIA prefix, non-empty
	// secret + token, expiration ~= 1h, and the AssumedRoleUser shape.
	beforeAssume := time.Now().UTC()
	aOut, err := stsCli.AssumeRole(&sts.AssumeRoleInput{
		RoleArn:         aws.String(roleARN),
		RoleSessionName: aws.String(stsSessionName),
	})
	require.NoError(t, err, "assume-role")

	creds := aOut.Credentials
	require.NotNil(t, creds, "AssumeRole returned nil Credentials")
	akid := aws.StringValue(creds.AccessKeyId)
	secret := aws.StringValue(creds.SecretAccessKey)
	token := aws.StringValue(creds.SessionToken)
	require.True(t, strings.HasPrefix(akid, "ASIA"), "session AKID must start with ASIA, got %q", akid)
	require.NotEmpty(t, secret, "empty SecretAccessKey")
	require.NotEmpty(t, token, "empty SessionToken")
	require.NotNil(t, creds.Expiration, "nil Expiration")

	// Default DurationSeconds is 3600 (1h). Bound the assertion loosely so a
	// few seconds of test-runner latency doesn't flake CI.
	expiresIn := creds.Expiration.Sub(beforeAssume)
	require.Greater(t, expiresIn, 30*time.Minute, "expiration too soon: %v (akid=%s)", expiresIn, akid)
	require.LessOrEqual(t, expiresIn, time.Hour+5*time.Minute, "expiration too far: %v (akid=%s)", expiresIn, akid)

	expectedAssumedARN := "arn:aws:sts::" + adminAccount + ":assumed-role/" + stsRoleName + "/" + stsSessionName
	require.NotNil(t, aOut.AssumedRoleUser, "nil AssumedRoleUser")
	require.Equal(t, expectedAssumedARN, aws.StringValue(aOut.AssumedRoleUser.Arn), "assumed-role ARN shape mismatch")
	assumedRoleID := aws.StringValue(aOut.AssumedRoleUser.AssumedRoleId)
	require.NotEmpty(t, assumedRoleID, "empty AssumedRoleId")
	require.True(t, strings.HasSuffix(assumedRoleID, ":"+stsSessionName), "AssumedRoleId must end with :%s, got %q", stsSessionName, assumedRoleID)

	// Drive the SigV4 ASIA path: use the freshly-minted creds against
	// GetCallerIdentity and assert the identity round-trips.
	sessionCli := gw.ClientsWithSessionCreds(t, akid, secret, token)
	sessWho, err := sessionCli.STS.GetCallerIdentity(&sts.GetCallerIdentityInput{})
	require.NoError(t, err, "get-caller-identity (assumed)")
	require.Equal(t, adminAccount, aws.StringValue(sessWho.Account), "assumed account mismatch")
	require.Equal(t, expectedAssumedARN, aws.StringValue(sessWho.Arn), "assumed ARN must be the sts:assumed-role form")
	require.Equal(t, assumedRoleID, aws.StringValue(sessWho.UserId), "UserId for assumed-role must equal AssumedRoleId")

	// Cross-service ASIA SigV4: the role carries no permission policy, so
	// gateway.checkPolicy resolves its (empty) managed policies and the
	// action falls to an implicit deny — assumability does not imply
	// permissions. The granted-policy positive case lives in
	// TestAssumedRoleControlPlaneEnforcement.
	_, err = sessionCli.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
	requireAWSErrorCode(t, err, "AccessDenied")

	// GetSessionToken is user-only: an assumed-role (ASIA) session must NOT
	// be able to mint a user session.
	_, err = sessionCli.STS.GetSessionToken(&sts.GetSessionTokenInput{})
	requireAWSErrorCode(t, err, "AccessDenied")

	// Tampered session token -> InvalidClientTokenId. Reuse the happy-path
	// ASIA akid+secret but present a forged X-Amz-Security-Token.
	// resolveSessionAKID verifies the token HMAC before the request
	// signature, so the mismatch surfaces as InvalidClientTokenId regardless
	// of the (valid) SigV4 sig. The probe is an EC2 call: the gateway
	// serialises SigV4 auth failures in the EC2 XML envelope, which the STS
	// Query client can't unmarshal (it masks the code as SerializationError).
	tamperedCli := gw.ClientsWithSessionCreds(t, akid, secret, token+"-tampered")
	_, err = tamperedCli.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
	requireAWSErrorCode(t, err, "InvalidClientTokenId")

	// Rejected-parameter wire rejections. The handler refuses inline session
	// policies, session tags, and MFA up front.
	_, err = stsCli.AssumeRole(&sts.AssumeRoleInput{
		RoleArn:         aws.String(roleARN),
		RoleSessionName: aws.String("reject-policy"),
		Policy:          aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`),
	})
	requireAWSErrorCode(t, err, "PackedPolicyTooLarge")

	_, err = stsCli.AssumeRole(&sts.AssumeRoleInput{
		RoleArn:         aws.String(roleARN),
		RoleSessionName: aws.String("reject-tags"),
		Tags:            []*sts.Tag{{Key: aws.String("team"), Value: aws.String("eng")}},
	})
	requireAWSErrorCode(t, err, "InvalidParameterValue")

	_, err = stsCli.AssumeRole(&sts.AssumeRoleInput{
		RoleArn:         aws.String(roleARN),
		RoleSessionName: aws.String("reject-mfa"),
		SerialNumber:    aws.String("arn:aws:iam::" + adminAccount + ":mfa/int"),
		TokenCode:       aws.String("123456"),
	})
	requireAWSErrorCode(t, err, "InvalidParameterValue")

	// DurationSeconds x role MaxSessionDuration clamp. With no
	// MaxSessionDuration on the role the ceiling is the 3600s default: 900s
	// mints a ~15m session and 7200s is rejected. After raising
	// MaxSessionDuration to 7200, 7200s mints a ~2h session and 10800s is
	// rejected.
	beforeShort := time.Now().UTC()
	shortOut, err := stsCli.AssumeRole(&sts.AssumeRoleInput{
		RoleArn:         aws.String(roleARN),
		RoleSessionName: aws.String("dur-900"),
		DurationSeconds: aws.Int64(900),
	})
	require.NoError(t, err, "assume-role DurationSeconds=900")
	require.NotNil(t, shortOut.Credentials, "nil Credentials for DurationSeconds=900")
	shortExpiresIn := shortOut.Credentials.Expiration.Sub(beforeShort)
	require.Greater(t, shortExpiresIn, 14*time.Minute, "DurationSeconds=900 expiry too soon: %v", shortExpiresIn)
	require.LessOrEqual(t, shortExpiresIn, 16*time.Minute, "DurationSeconds=900 expiry too far: %v", shortExpiresIn)

	_, err = stsCli.AssumeRole(&sts.AssumeRoleInput{
		RoleArn:         aws.String(roleARN),
		RoleSessionName: aws.String("dur-7200-over"),
		DurationSeconds: aws.Int64(7200),
	})
	requireAWSErrorCode(t, err, "ValidationError")

	_, err = iamCli.UpdateRole(&iam.UpdateRoleInput{
		RoleName:           aws.String(stsRoleName),
		MaxSessionDuration: aws.Int64(7200),
	})
	require.NoError(t, err, "update-role MaxSessionDuration=7200")

	beforeLong := time.Now().UTC()
	longOut, err := stsCli.AssumeRole(&sts.AssumeRoleInput{
		RoleArn:         aws.String(roleARN),
		RoleSessionName: aws.String("dur-7200-ok"),
		DurationSeconds: aws.Int64(7200),
	})
	require.NoError(t, err, "assume-role DurationSeconds=7200 after raise")
	require.NotNil(t, longOut.Credentials, "nil Credentials for DurationSeconds=7200")
	longExpiresIn := longOut.Credentials.Expiration.Sub(beforeLong)
	require.Greater(t, longExpiresIn, time.Hour+50*time.Minute, "DurationSeconds=7200 expiry too soon: %v", longExpiresIn)
	require.LessOrEqual(t, longExpiresIn, 2*time.Hour+5*time.Minute, "DurationSeconds=7200 expiry too far: %v", longExpiresIn)

	_, err = stsCli.AssumeRole(&sts.AssumeRoleInput{
		RoleArn:         aws.String(roleARN),
		RoleSessionName: aws.String("dur-10800-over"),
		DurationSeconds: aws.Int64(10800),
	})
	requireAWSErrorCode(t, err, "ValidationError")

	// Chained assume: create a second role whose trust policy names the
	// first role's IAM ARN, then have the assumed-role session call
	// AssumeRole on it. Exercises the role-ARN-clause vs. session-ARN-caller
	// auto-expansion path end-to-end.
	chainedTrustPolicy := fmt.Sprintf(
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":%q},"Action":"sts:AssumeRole"}]}`,
		iamRoleARN(adminAccount, stsRoleName),
	)
	chainCreateOut, err := iamCli.CreateRole(&iam.CreateRoleInput{
		RoleName:                 aws.String(stsRoleNameChain),
		AssumeRolePolicyDocument: aws.String(chainedTrustPolicy),
		Description:              aws.String("integration test STS chained AssumeRole target"),
	})
	require.NoError(t, err, "create-role (chain)")
	chainedRoleARN := aws.StringValue(chainCreateOut.Role.Arn)

	chainOut, err := sessionCli.STS.AssumeRole(&sts.AssumeRoleInput{
		RoleArn:         aws.String(chainedRoleARN),
		RoleSessionName: aws.String(stsSessionNameChain),
	})
	require.NoError(t, err, "assume-role (chain)")
	require.NotNil(t, chainOut.Credentials, "chained AssumeRole returned nil Credentials")
	chainAKID := aws.StringValue(chainOut.Credentials.AccessKeyId)
	chainSecret := aws.StringValue(chainOut.Credentials.SecretAccessKey)
	chainToken := aws.StringValue(chainOut.Credentials.SessionToken)
	require.True(t, strings.HasPrefix(chainAKID, "ASIA"), "chained session AKID must start with ASIA, got %q", chainAKID)
	require.NotEmpty(t, chainSecret, "empty chained SecretAccessKey")
	require.NotEmpty(t, chainToken, "empty chained SessionToken")

	expectedChainARN := "arn:aws:sts::" + adminAccount + ":assumed-role/" + stsRoleNameChain + "/" + stsSessionNameChain
	require.NotNil(t, chainOut.AssumedRoleUser, "nil chained AssumedRoleUser")
	require.Equal(t, expectedChainARN, aws.StringValue(chainOut.AssumedRoleUser.Arn), "chained assumed-role ARN shape mismatch")

	// Round-trip the chained creds to prove the new session token verifies
	// on the wire and the gateway reports the chained-role ARN, not the
	// source-role ARN.
	chainCli := gw.ClientsWithSessionCreds(t, chainAKID, chainSecret, chainToken)
	chainWho, err := chainCli.STS.GetCallerIdentity(&sts.GetCallerIdentityInput{})
	require.NoError(t, err, "get-caller-identity (chained)")
	require.Equal(t, expectedChainARN, aws.StringValue(chainWho.Arn), "chained GetCallerIdentity must report the chained-role ARN, not the source-role ARN")

	// Trust-policy denial: swap to a principal that cannot match the caller,
	// then re-assume to confirm the trust-policy evaluator returns
	// AccessDenied.
	_, err = iamCli.UpdateAssumeRolePolicy(&iam.UpdateAssumeRolePolicyInput{
		RoleName:       aws.String(stsRoleName),
		PolicyDocument: aws.String(stsTrustPolicyDenyAny),
	})
	require.NoError(t, err, "update-assume-role-policy")
	_, err = stsCli.AssumeRole(&sts.AssumeRoleInput{
		RoleArn:         aws.String(roleARN),
		RoleSessionName: aws.String("session-2"),
	})
	requireAWSErrorCode(t, err, "AccessDenied")

	// All missing-role lookups are masked to AccessDenied by the handler,
	// matching AWS and preventing role enumeration.
	_, err = stsCli.AssumeRole(&sts.AssumeRoleInput{
		RoleArn:         aws.String(iamRoleARN(adminAccount, "sts-no-such-role")),
		RoleSessionName: aws.String("ghost"),
	})
	requireAWSErrorCode(t, err, "AccessDenied")

	// Write-time trust-policy rejection via the SDK: Condition,
	// NotPrincipal, and NotAction must be refused at CreateRole rather than
	// silently accepted (silent-allow vector).
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
		_, err := iamCli.CreateRole(&iam.CreateRoleInput{
			RoleName:                 aws.String(tc.roleName),
			AssumeRolePolicyDocument: aws.String(tc.doc),
		})
		requireAWSErrorCode(t, err, "MalformedPolicyDocument")
	}

	// Assertive teardown. The chained role tears down first so its
	// trust-policy dependency on stsRoleName doesn't matter, but DeleteRole
	// is unaffected by trust-policy references in either direction.
	_, err = iamCli.DeleteRole(&iam.DeleteRoleInput{RoleName: aws.String(stsRoleNameChain)})
	require.NoError(t, err, "delete-role (chain)")
	_, err = iamCli.DeleteRole(&iam.DeleteRoleInput{RoleName: aws.String(stsRoleName)})
	require.NoError(t, err, "delete-role")
}
