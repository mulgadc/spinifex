package sts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
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

// recordingKV captures every write to the sessions bucket, so a test sees the
// first Create itself rather than the one revision the bucket keeps.
type recordingKV struct {
	jetstream.KeyValue

	mu         sync.Mutex
	creates    [][]byte
	rewrites   int
	failCreate error
}

func (r *recordingKV) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	r.mu.Lock()
	r.creates = append(r.creates, bytes.Clone(value))
	fail := r.failCreate
	r.mu.Unlock()
	if fail != nil {
		return 0, fail
	}
	return r.KeyValue.Create(ctx, key, value, opts...)
}

func (r *recordingKV) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	r.rewrite()
	return r.KeyValue.Put(ctx, key, value)
}

func (r *recordingKV) PutString(ctx context.Context, key, value string) (uint64, error) {
	r.rewrite()
	return r.KeyValue.PutString(ctx, key, value)
}

func (r *recordingKV) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	r.rewrite()
	return r.KeyValue.Update(ctx, key, value, revision)
}

func (r *recordingKV) rewrite() {
	r.mu.Lock()
	r.rewrites++
	r.mu.Unlock()
}

func installRecorder(t *testing.T, svc *STSServiceImpl) *recordingKV {
	t.Helper()
	rec := &recordingKV{KeyValue: sessionsKV(t, svc)}
	svc.sessions = &kvstore.Store[SessionCredential]{Bucket: kvstore.NewOpenBucket(nil, rec, sessionCredentialsConfig())}
	return rec
}

// The labels are part of the create-only write: the one Create carries them and
// nothing rewrites the record afterwards.
func TestAssumeRoleForLambdaEnvironment_FirstRecordCarriesLabels(t *testing.T) {
	svc, _ := newTestSetup(t)
	role := createRoleInAccount(t, svc, testCallerAccountID, testLambdaRoleName, lambdaTrustPolicyDoc)
	rec := installRecorder(t, svc)

	_, err := svc.AssumeRoleForLambdaEnvironment(t.Context(), lambdaEnvRequest(*role.Arn))
	require.NoError(t, err)

	require.Len(t, rec.creates, 1)
	assert.Zero(t, rec.rewrites)
	var first SessionCredential
	require.NoError(t, json.Unmarshal(rec.creates[0], &first))
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
		{"function ARN without region", func(r *LambdaEnvironmentSessionRequest) {
			r.FunctionARN = "arn:aws:lambda::" + testCallerAccountID + ":function:hello"
		}, awserrors.ErrorValidationError},
		{"function ARN in another partition", func(r *LambdaEnvironmentSessionRequest) {
			r.FunctionARN = "arn:aws-cn:lambda:cn-north-1:" + testCallerAccountID + ":function:hello"
		}, awserrors.ErrorValidationError},
		{"function name with slash", func(r *LambdaEnvironmentSessionRequest) { r.FunctionARN = testLambdaFunctionARN + "/world" }, awserrors.ErrorValidationError},
		{"layer ARN", func(r *LambdaEnvironmentSessionRequest) {
			r.FunctionARN = "arn:aws:lambda:ap-southeast-2:" + testCallerAccountID + ":layer:hello"
		}, awserrors.ErrorValidationError},
		{"function ID bad character", func(r *LambdaEnvironmentSessionRequest) { r.FunctionID = "f/1" }, awserrors.ErrorValidationError},
		{"function ID too long", func(r *LambdaEnvironmentSessionRequest) { r.FunctionID = strings.Repeat("f", 65) }, awserrors.ErrorValidationError},
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

// The issuer's local name, region and function ID checks accept exactly what the
// Lambda function owner's Create accepts.
func TestValidateLambdaEnvironmentRequest_ConformsToFunctionOwner(t *testing.T) {
	const acct = "000000000001"
	long := func(c string, n int) string { return strings.Repeat(c, n) }
	cases := []struct {
		name, region, functionID string
		accepted                 bool
	}{
		{"hello", "ap-southeast-2", "f1", true},
		{"Hello_World-1", "us-east-1", testLambdaFunctionID, true},
		{long("n", 64), "a", long("i", 64), true},
		{"ids", "ap-southeast-2", "a_b-C", true},
		{"hello/world", "ap-southeast-2", "f1", false},
		{"hello.world", "ap-southeast-2", "f1", false},
		{long("n", 65), "ap-southeast-2", "f1", false},
		{"c1", "", "f1", false},
		{"c2", "AP-SOUTHEAST-2", "f1", false},
		{"c3", "ap_southeast_2", "f1", false},
		{"c4", long("r", 33), "f1", false},
		{"c5", "ap-southeast-2", long("i", 65), false},
		{"c6", "ap-southeast-2", "f/1", false},
		{"c7", "ap-southeast-2", "f.1", false},
	}
	_, nc, _ := testutil.StartTestJetStream(t)
	owner := function.NewMetaService(testutil.NewJetStream(t, nc))
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%.20s/%.20s/%.20s", tc.name, tc.region, tc.functionID), func(t *testing.T) {
			_, ownerErr := owner.Create(t.Context(), &function.CreateRequest{Region: tc.region, Record: function.Record{
				FunctionID: tc.functionID, FunctionName: tc.name, Role: "arn:aws:iam::" + acct + ":role/r",
				Runtime: "provided.al2023", Handler: "bootstrap", Timeout: 3, MemorySize: 128,
				Code: function.Code{RevisionID: "rev1"},
			}}, acct)
			_, issuerErr := validateLambdaEnvironmentRequest(LambdaEnvironmentSessionRequest{
				AccountID:          acct,
				RoleARN:            "arn:aws:iam::" + acct + ":role/r",
				FunctionARN:        fmt.Sprintf("arn:aws:lambda:%s:%s:function:%s", tc.region, acct, tc.name),
				FunctionID:         tc.functionID,
				EnvironmentID:      testLambdaEnvID,
				MaxLifetimeSeconds: 3600,
			})
			assert.Equal(t, tc.accepted, ownerErr == nil, "owner: %v", ownerErr)
			assert.Equal(t, tc.accepted, issuerErr == nil, "issuer: %v", issuerErr)
		})
	}
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

// tokenShape matches a base64 session token. The failed mint's token is never
// observable, so absence of this shape is limited evidence, not proof.
var tokenShape = regexp.MustCompile(`[A-Za-z0-9+/]{43}=`)

func TestAssumeRoleForLambdaEnvironment_FailedMintIssuesNothingAndHidesSecret(t *testing.T) {
	svc, nc := newTestSetup(t)
	role := createRoleInAccount(t, svc, testCallerAccountID, testLambdaRoleName, lambdaTrustPolicyDoc)
	rec := installRecorder(t, svc)
	rec.failCreate = errors.New("injected create failure")

	var logs bytes.Buffer
	captureSlog(t, &logs)
	out, err := svc.AssumeRoleForLambdaEnvironment(t.Context(), lambdaEnvRequest(*role.Arn))
	require.Error(t, err)
	assert.Nil(t, out)
	assert.Zero(t, sessionCount(t, svc, nc))

	// The secret generated for the failed write is recovered from the record it tried.
	require.Len(t, rec.creates, 1)
	var attempted SessionCredential
	require.NoError(t, json.Unmarshal(rec.creates[0], &attempted))
	secret, err2 := iamdomain.DecryptSecret(attempted.SecretEncrypted, svc.masterKey)
	require.NoError(t, err2)
	require.NotEmpty(t, secret)
	for _, observed := range []string{err.Error(), logs.String()} {
		assert.NotContains(t, observed, secret)
		assert.NotContains(t, observed, attempted.SessionTokenHMAC)
		assert.False(t, tokenShape.MatchString(observed), "token-shaped value in %q", observed)
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

	entry, err := sessionsKV(t, svc).Get(t.Context(), aws.StringValue(out.Credentials.AccessKeyId))
	require.NoError(t, err)
	require.Contains(t, logs.String(), "AssumeRoleForLambdaEnvironment success")
	for _, plain := range []string{secret, token} {
		assert.NotContains(t, string(entry.Value()), plain)
		assert.NotContains(t, logs.String(), plain)
	}
}
