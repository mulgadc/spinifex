package sts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testLambdaFunctionID  = "0123456789abcdef0123456789abcdef"
	testLambdaEnvID       = "env-0001"
	testLambdaRoleName    = "fn-exec"
	testLambdaFunctionARN = "arn:aws:lambda:ap-southeast-2:" + testCallerAccountID + ":function:hello"
)

func lambdaEnvRequest(roleARN string) LambdaEnvironmentSessionRequest {
	return LambdaEnvironmentSessionRequest{
		AccountID:          testCallerAccountID,
		RoleARN:            roleARN,
		FunctionARN:        testLambdaFunctionARN,
		FunctionID:         testLambdaFunctionID,
		EnvironmentID:      testLambdaEnvID,
		MaxLifetimeSeconds: 3600,
	}
}

func lambdaTrustWithSourceAccount(effect, accountID string) string {
	return fmt.Sprintf(`{"Effect":%q,"Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole","Condition":{"StringEquals":{"aws:SourceAccount":%q}}}`, effect, accountID)
}

func trustDoc(statements ...string) string {
	return `{"Version":"2012-10-17","Statement":[` + strings.Join(statements, ",") + `]}`
}

func TestAssumeRoleForLambdaEnvironment_IssuesUsableCredential(t *testing.T) {
	svc, _ := newTestSetup(t)
	role := createRoleInAccount(t, svc, testCallerAccountID, testLambdaRoleName, lambdaTrustPolicyDoc)

	out, err := svc.AssumeRoleForLambdaEnvironment(t.Context(), lambdaEnvRequest(*role.Arn))
	require.NoError(t, err)

	akid := aws.StringValue(out.Credentials.AccessKeyId)
	assert.True(t, strings.HasPrefix(akid, SessionAccessKeyIDPrefix))
	assert.Equal(t, fmt.Sprintf("arn:aws:sts::%s:assumed-role/%s/%s", testCallerAccountID, testLambdaRoleName, testLambdaEnvID),
		aws.StringValue(out.AssumedRoleUser.Arn))
	assert.Equal(t, aws.StringValue(role.RoleId)+":"+testLambdaEnvID, aws.StringValue(out.AssumedRoleUser.AssumedRoleId))

	stored, err := svc.LookupSessionCredential(akid)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.True(t, svc.VerifySessionToken(stored, aws.StringValue(out.Credentials.SessionToken)))
	secret, err := iamdomain.DecryptSecret(stored.SecretEncrypted, svc.masterKey)
	require.NoError(t, err)
	assert.Equal(t, aws.StringValue(out.Credentials.SecretAccessKey), secret)
	_, err = svc.VerifySessionPrincipal(stored)
	require.NoError(t, err)
	assert.WithinDuration(t, stored.ExpiresAt, aws.TimeValue(out.Credentials.Expiration), 0)
}

// The labels are part of the create-only write, so no revision of the record
// exists without them.
func TestAssumeRoleForLambdaEnvironment_FirstRecordCarriesLabels(t *testing.T) {
	svc, _ := newTestSetup(t)
	role := createRoleInAccount(t, svc, testCallerAccountID, testLambdaRoleName, lambdaTrustPolicyDoc)

	out, err := svc.AssumeRoleForLambdaEnvironment(t.Context(), lambdaEnvRequest(*role.Arn))
	require.NoError(t, err)

	history, err := sessionsKV(t, svc).History(t.Context(), aws.StringValue(out.Credentials.AccessKeyId))
	require.NoError(t, err)
	require.Len(t, history, 1)
	var first SessionCredential
	require.NoError(t, json.Unmarshal(history[0].Value(), &first))
	assert.Equal(t, testLambdaFunctionID, first.LambdaFunctionID)
	assert.Equal(t, testLambdaFunctionARN, first.LambdaFunctionARN)
	assert.Equal(t, testLambdaEnvID, first.LambdaEnvironmentID)
	assert.Equal(t, PrincipalTypeAssumedRole, first.PrincipalType)
	assert.Equal(t, aws.StringValue(role.RoleId), first.RoleID)
	assert.Equal(t, testCallerAccountID, first.AccountID)
}

func TestAssumeRoleForLambdaEnvironment_RefusesWithoutLambdaTrust(t *testing.T) {
	cases := map[string]string{
		"ec2 only":                  trustPolicyAllowingEC2Service(),
		"user only":                 trustPolicyAllowingUser(testCallerARN()),
		"lambda for another action": `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:TagSession"}]}`,
	}
	for name, trust := range cases {
		t.Run(name, func(t *testing.T) {
			svc, nc := newTestSetup(t)
			role := createRoleInAccount(t, svc, testCallerAccountID, testLambdaRoleName, trust)

			out, err := svc.AssumeRoleForLambdaEnvironment(t.Context(), lambdaEnvRequest(*role.Arn))
			assert.Nil(t, out)
			assertErrorCode(t, err, awserrors.ErrorAccessDenied)
			assert.Zero(t, sessionCount(t, svc, nc))
		})
	}
}

// A role in another account and a role that does not exist give the same answer.
func TestAssumeRoleForLambdaEnvironment_RefusesForeignOrMissingRole(t *testing.T) {
	svc, nc := newTestSetup(t)
	foreign := createRoleInAccount(t, svc, testCrossAccountID, testLambdaRoleName, lambdaTrustPolicyDoc)

	_, foreignErr := svc.AssumeRoleForLambdaEnvironment(t.Context(), lambdaEnvRequest(*foreign.Arn))
	assertErrorCode(t, foreignErr, awserrors.ErrorAccessDenied)

	missingARN := fmt.Sprintf("arn:aws:iam::%s:role/ghost", testCallerAccountID)
	_, missingErr := svc.AssumeRoleForLambdaEnvironment(t.Context(), lambdaEnvRequest(missingARN))
	assertErrorCode(t, missingErr, awserrors.ErrorAccessDenied)

	assert.Equal(t, foreignErr.Error(), missingErr.Error())
	assert.Zero(t, sessionCount(t, svc, nc))
}

func TestAssumeRoleForLambdaEnvironment_ValidatesRequest(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*LambdaEnvironmentSessionRequest)
		code   string
	}{
		{"missing account", func(r *LambdaEnvironmentSessionRequest) { r.AccountID = "" }, awserrors.ErrorMissingParameter},
		{"missing role", func(r *LambdaEnvironmentSessionRequest) { r.RoleARN = "" }, awserrors.ErrorMissingParameter},
		{"missing function ARN", func(r *LambdaEnvironmentSessionRequest) { r.FunctionARN = "" }, awserrors.ErrorMissingParameter},
		{"missing function ID", func(r *LambdaEnvironmentSessionRequest) { r.FunctionID = "" }, awserrors.ErrorMissingParameter},
		{"missing environment ID", func(r *LambdaEnvironmentSessionRequest) { r.EnvironmentID = "" }, awserrors.ErrorMissingParameter},
		{"function ARN in another account", func(r *LambdaEnvironmentSessionRequest) {
			r.FunctionARN = "arn:aws:lambda:ap-southeast-2:" + testCrossAccountID + ":function:hello"
		}, awserrors.ErrorValidationError},
		{"malformed function ARN", func(r *LambdaEnvironmentSessionRequest) { r.FunctionARN = "not-an-arn" }, awserrors.ErrorValidationError},
		{"function ARN of another service", func(r *LambdaEnvironmentSessionRequest) {
			r.FunctionARN = "arn:aws:iam::" + testCallerAccountID + ":function:hello"
		}, awserrors.ErrorValidationError},
		{"qualified function ARN", func(r *LambdaEnvironmentSessionRequest) { r.FunctionARN = testLambdaFunctionARN + ":1" }, awserrors.ErrorValidationError},
		{"layer ARN", func(r *LambdaEnvironmentSessionRequest) {
			r.FunctionARN = "arn:aws:lambda:ap-southeast-2:" + testCallerAccountID + ":layer:hello"
		}, awserrors.ErrorValidationError},
		{"function ID uppercase", func(r *LambdaEnvironmentSessionRequest) { r.FunctionID = strings.ToUpper(testLambdaFunctionID) }, awserrors.ErrorValidationError},
		{"function ID short", func(r *LambdaEnvironmentSessionRequest) { r.FunctionID = testLambdaFunctionID[:31] }, awserrors.ErrorValidationError},
		{"environment ID too short", func(r *LambdaEnvironmentSessionRequest) { r.EnvironmentID = "e" }, awserrors.ErrorValidationError},
		{"environment ID bad character", func(r *LambdaEnvironmentSessionRequest) { r.EnvironmentID = "env/1" }, awserrors.ErrorValidationError},
		{"environment ID too long", func(r *LambdaEnvironmentSessionRequest) { r.EnvironmentID = strings.Repeat("e", 65) }, awserrors.ErrorValidationError},
	}
	svc, nc := newTestSetup(t)
	role := createRoleInAccount(t, svc, testCallerAccountID, testLambdaRoleName, lambdaTrustPolicyDoc)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := lambdaEnvRequest(*role.Arn)
			tc.mutate(&req)
			out, err := svc.AssumeRoleForLambdaEnvironment(t.Context(), req)
			assert.Nil(t, out)
			assertErrorCode(t, err, tc.code)
		})
	}
	assert.Zero(t, sessionCount(t, svc, nc))
}

// The role's MaxSessionDuration does not bound a service-assumed session, so a
// one-hour environment issues against a default one-hour role.
func TestAssumeRoleForLambdaEnvironment_Lifetime(t *testing.T) {
	svc, nc := newTestSetup(t)
	role := createRoleInAccount(t, svc, testCallerAccountID, testLambdaRoleName, lambdaTrustPolicyDoc)
	require.Contains(t, []int64{0, defaultDurationSeconds}, aws.Int64Value(role.MaxSessionDuration), "role must have the default limit")

	issued := []struct {
		name     string
		lifetime int64
		want     int64
	}{
		{"one hour on a default role", 3600, 4560},
		{"one second", 1, 961},
		{"at the ceiling", maxDurationSeconds - 960, maxDurationSeconds},
	}
	for _, tc := range issued {
		t.Run(tc.name, func(t *testing.T) {
			req := lambdaEnvRequest(*role.Arn)
			req.MaxLifetimeSeconds = tc.lifetime
			out, err := svc.AssumeRoleForLambdaEnvironment(t.Context(), req)
			require.NoError(t, err)
			stored, err := svc.LookupSessionCredential(aws.StringValue(out.Credentials.AccessKeyId))
			require.NoError(t, err)
			assert.Equal(t, time.Duration(tc.want)*time.Second, stored.ExpiresAt.Sub(stored.CreatedAt))
		})
	}
	before := sessionCount(t, svc, nc)

	for _, lifetime := range []int64{0, -1, maxDurationSeconds - 959, math.MaxInt64, math.MinInt64} {
		t.Run(fmt.Sprintf("refused %d", lifetime), func(t *testing.T) {
			req := lambdaEnvRequest(*role.Arn)
			req.MaxLifetimeSeconds = lifetime
			out, err := svc.AssumeRoleForLambdaEnvironment(t.Context(), req)
			assert.Nil(t, out)
			assertErrorCode(t, err, awserrors.ErrorValidationError)
		})
	}
	assert.Equal(t, before, sessionCount(t, svc, nc))
}

func TestAssumeRoleForLambdaEnvironment_StorageFailureIssuesNothing(t *testing.T) {
	svc, nc := newTestSetup(t)
	role := createRoleInAccount(t, svc, testCallerAccountID, testLambdaRoleName, lambdaTrustPolicyDoc)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	require.NoError(t, js.DeleteKeyValue(t.Context(), KVBucketSessionCredentials))

	var logs bytes.Buffer
	captureSlog(t, &logs)
	out, err := svc.AssumeRoleForLambdaEnvironment(t.Context(), lambdaEnvRequest(*role.Arn))
	require.Error(t, err)
	assert.Nil(t, out)
	for _, field := range []string{"secret_encrypted", "session_token_hmac"} {
		assert.NotContains(t, err.Error(), field)
		assert.NotContains(t, logs.String(), field)
	}
}

func TestAssumeRoleForLambdaEnvironment_InactiveAccountRefused(t *testing.T) {
	svc, nc := newTestSetup(t)
	role := createRoleInAccount(t, svc, testCallerAccountID, testLambdaRoleName, lambdaTrustPolicyDoc)
	putTestAccount(t, nc, testCallerAccountID, iamdomain.AccountStatusSuspended)

	out, err := svc.AssumeRoleForLambdaEnvironment(t.Context(), lambdaEnvRequest(*role.Arn))
	assert.Nil(t, out)
	assertErrorCode(t, err, awserrors.ErrorAccessDenied)
	assert.Zero(t, sessionCount(t, svc, nc))
}

// Every other minting path leaves the Lambda labels out of the record entirely.
func TestMintSession_OtherPathsWriteNoLambdaLabels(t *testing.T) {
	svc, _ := newTestSetup(t)
	caller := testCallerARN()
	var akids []string

	userRole := createRoleInAccount(t, svc, testCallerAccountID, "app", trustPolicyAllowingUser(caller))
	ar, err := svc.AssumeRole(testCallerAccountID, caller, testCallerUserName, basicAssumeRoleInput(*userRole.Arn, "s1"))
	require.NoError(t, err)
	akids = append(akids, aws.StringValue(ar.Credentials.AccessKeyId))

	ec2Role := createRoleInAccount(t, svc, testCallerAccountID, "inst", trustPolicyAllowingEC2Service())
	ir, err := svc.AssumeRoleForInstance(testCallerAccountID, *ec2Role.Arn, testInstanceID, defaultDurationSeconds)
	require.NoError(t, err)
	akids = append(akids, aws.StringValue(ir.Credentials.AccessKeyId))

	createRoleInAccount(t, svc, testCallerAccountID, CodeStoreRoleName(testCodeStoreRegion), lambdaTrustPolicyDoc)
	cr, err := svc.AssumeCodeStoreRole(testCodeStoreRegion, "node-1")
	require.NoError(t, err)
	akids = append(akids, aws.StringValue(cr.Credentials.AccessKeyId))

	seedUser(t, svc, testCallerAccountID, testCallerUserName)
	gt, err := svc.GetSessionToken(testCallerAccountID, testCallerUserName, principalTypeUser,
		testCallerAccessKeyID, &sts.GetSessionTokenInput{})
	require.NoError(t, err)
	akids = append(akids, aws.StringValue(gt.Credentials.AccessKeyId))

	f := newWebIdentityFixture(t, svc, testCallerAccountID)
	wiRole := createRoleForFixture(t, svc, testCallerAccountID, "irsa-app",
		webIdentityTrustPolicy(f.federatedARN, f.issuer, "system:serviceaccount:default:my-sa"))
	wi, err := svc.AssumeRoleWithWebIdentity(&sts.AssumeRoleWithWebIdentityInput{
		RoleArn:          wiRole.Arn,
		RoleSessionName:  aws.String("pod-1"),
		WebIdentityToken: aws.String(f.signToken(f.defaultClaims())),
	})
	require.NoError(t, err)
	akids = append(akids, aws.StringValue(wi.Credentials.AccessKeyId))

	kv := sessionsKV(t, svc)
	for _, akid := range akids {
		entry, err := kv.Get(t.Context(), akid)
		require.NoError(t, err)
		assert.NotContains(t, string(entry.Value()), "lambda_", "record %s", akid)
	}
}

// The issuer's answer for an aws:SourceAccount-conditioned trust matches RoleTrusts,
// which other Lambda paths use to check the same role.
func TestAssumeRoleForLambdaEnvironment_SourceAccountMatchesRoleTrusts(t *testing.T) {
	cases := []struct {
		name  string
		trust string
		allow bool
	}{
		{"allow for own account", trustDoc(lambdaTrustWithSourceAccount("Allow", testCallerAccountID)), true},
		{"allow for another account", trustDoc(lambdaTrustWithSourceAccount("Allow", testCrossAccountID)), false},
		{"deny for own account over unconditional allow", trustDoc(
			`{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}`,
			lambdaTrustWithSourceAccount("Deny", testCallerAccountID)), false},
		{"deny for another account over unconditional allow", trustDoc(
			`{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}`,
			lambdaTrustWithSourceAccount("Deny", testCrossAccountID)), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newTestSetup(t)
			role := createRoleInAccount(t, svc, testCallerAccountID, testLambdaRoleName, tc.trust)

			trusted, err := RoleTrusts(svc.iamSvc, lambdaServicePrincipal)(testCallerAccountID, *role.Arn)
			require.NoError(t, err)
			assert.Equal(t, tc.allow, trusted)

			_, err = svc.AssumeRoleForLambdaEnvironment(t.Context(), lambdaEnvRequest(*role.Arn))
			if tc.allow {
				require.NoError(t, err)
				return
			}
			assertErrorCode(t, err, awserrors.ErrorAccessDenied)
		})
	}
}

// swappingRoles answers the first GetRole with first and every later one with
// later, as if the role were replaced between two reads.
type swappingRoles struct {
	iamdomain.IAMService
	first, later *iam.Role
	calls        int
}

func (s *swappingRoles) GetRole(string, *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	s.calls++
	if s.calls == 1 {
		return &iam.GetRoleOutput{Role: s.first}, nil
	}
	return &iam.GetRoleOutput{Role: s.later}, nil
}

func TestAssumeRoleForLambdaEnvironment_SingleRoleResolution(t *testing.T) {
	trusting := &iam.Role{
		RoleName: aws.String(testLambdaRoleName), RoleId: aws.String("AROATRUSTSLAMBDA0001"),
		Arn:                      aws.String(fmt.Sprintf("arn:aws:iam::%s:role/%s", testCallerAccountID, testLambdaRoleName)),
		AssumeRolePolicyDocument: aws.String(lambdaTrustPolicyDoc),
	}
	untrusting := &iam.Role{
		RoleName: trusting.RoleName, RoleId: aws.String("AROAREPLACEDROLE0002"), Arn: trusting.Arn,
		AssumeRolePolicyDocument: aws.String(trustPolicyAllowingEC2Service()),
	}

	t.Run("trusting role read first", func(t *testing.T) {
		svc, _ := newTestSetup(t)
		swap := &swappingRoles{IAMService: svc.iamSvc, first: trusting, later: untrusting}
		svc.iamSvc = swap

		out, err := svc.AssumeRoleForLambdaEnvironment(t.Context(), lambdaEnvRequest(*trusting.Arn))
		require.NoError(t, err)
		stored, err := svc.LookupSessionCredential(aws.StringValue(out.Credentials.AccessKeyId))
		require.NoError(t, err)
		assert.Equal(t, "AROATRUSTSLAMBDA0001", stored.RoleID)
		assert.Equal(t, 1, swap.calls)
	})

	t.Run("untrusting role read first", func(t *testing.T) {
		svc, nc := newTestSetup(t)
		swap := &swappingRoles{IAMService: svc.iamSvc, first: untrusting, later: trusting}
		svc.iamSvc = swap

		_, err := svc.AssumeRoleForLambdaEnvironment(t.Context(), lambdaEnvRequest(*trusting.Arn))
		assertErrorCode(t, err, awserrors.ErrorAccessDenied)
		assert.Equal(t, 1, swap.calls)
		assert.Zero(t, sessionCount(t, svc, nc))
	})
}

func captureSlog(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

func TestAssumeRoleForLambdaEnvironment_NoPlaintextAtRestOrInLogs(t *testing.T) {
	svc, _ := newTestSetup(t)
	role := createRoleInAccount(t, svc, testCallerAccountID, testLambdaRoleName, lambdaTrustPolicyDoc)

	var logs bytes.Buffer
	captureSlog(t, &logs)
	out, err := svc.AssumeRoleForLambdaEnvironment(t.Context(), lambdaEnvRequest(*role.Arn))
	require.NoError(t, err)
	secret := aws.StringValue(out.Credentials.SecretAccessKey)
	token := aws.StringValue(out.Credentials.SessionToken)
	require.NotEmpty(t, secret)
	require.NotEmpty(t, token)

	// Refusals after a success must not echo the earlier credential either.
	req := lambdaEnvRequest(*role.Arn)
	req.MaxLifetimeSeconds = 0
	_, refusal := svc.AssumeRoleForLambdaEnvironment(t.Context(), req)
	require.Error(t, refusal)

	entry, err := sessionsKV(t, svc).Get(t.Context(), aws.StringValue(out.Credentials.AccessKeyId))
	require.NoError(t, err)
	require.Contains(t, logs.String(), "AssumeRoleForLambdaEnvironment success")
	for _, plain := range []string{secret, token} {
		assert.NotContains(t, string(entry.Value()), plain)
		assert.NotContains(t, logs.String(), plain)
		assert.NotContains(t, refusal.Error(), plain)
	}
}
