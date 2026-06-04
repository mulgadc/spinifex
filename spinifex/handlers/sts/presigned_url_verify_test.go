package handlers_sts

import (
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	v4 "github.com/aws/aws-sdk-go/aws/signer/v4"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testPresignedHost   = "sts.au-mel-1.mulga.local"
	testPresignedRegion = "au-mel-1"
)

// presignTestURL builds an `aws eks get-token`-shape SigV4-presigned GET URL
// for sts.GetCallerIdentity, signing the X-K8s-Aws-Id header with the given
// clusterName so a Verify call against the same clusterName succeeds. Hand-
// rolled rather than via aws-sdk-go-v2 so the test does not pull an extra
// dependency just to assert the production reconstruction.
func presignTestURL(t *testing.T, accessKeyID, secret, clusterName string, signedTime time.Time, expires int64) string {
	t.Helper()
	host := testPresignedHost
	region := testPresignedRegion
	service := "sts"
	terminator := "aws4_request"
	date := signedTime.UTC().Format("20060102")
	amzDate := signedTime.UTC().Format("20060102T150405Z")
	credentialScope := strings.Join([]string{date, region, service, terminator}, "/")

	q := url.Values{}
	q.Set("Action", "GetCallerIdentity")
	q.Set("Version", "2011-06-15")
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", accessKeyID+"/"+credentialScope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", strconv.FormatInt(expires, 10))
	q.Set("X-Amz-SignedHeaders", "host;x-k8s-aws-id")

	// Canonical query string (no signature yet) — must match the
	// production reconstruction, byte-for-byte.
	canonQ := canonicalQueryStringForTest(q)

	canonHeaders := "host:" + host + "\n" + "x-k8s-aws-id:" + clusterName + "\n"
	signedHeadersList := "host;x-k8s-aws-id"
	canonRequest := strings.Join([]string{
		"GET", "/", canonQ, canonHeaders, signedHeadersList, emptyStringSHA256,
	}, "\n")

	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, credentialScope, hexSHA256(canonRequest),
	}, "\n")

	key := deriveSigningKey(secret, date, region, service)
	sig := hex.EncodeToString(hmacSHA256(key, stringToSign))
	q.Set("X-Amz-Signature", sig)

	return "https://" + host + "/?" + canonicalQueryStringForTest(q)
}

// canonicalQueryStringForTest mirrors the production canonical-query
// implementation so the test signer agrees with the verifier on
// canonicalisation. Sorting + encoding rules must stay aligned; if either
// implementation drifts, signature verification breaks immediately —
// caught by TestVerifyPresignedGetCallerIdentity_HappyPath.
func canonicalQueryStringForTest(q url.Values) string {
	type kv struct{ k, v string }
	pairs := make([]kv, 0, len(q))
	for k, vs := range q {
		ek := awsQueryEscape(k)
		for _, v := range vs {
			pairs = append(pairs, kv{ek, awsQueryEscape(v)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k == pairs[j].k {
			return pairs[i].v < pairs[j].v
		}
		return pairs[i].k < pairs[j].k
	})
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = p.k + "=" + p.v
	}
	return strings.Join(parts, "&")
}

// seedAccessKey writes an active IAM access key for the user and returns its
// plaintext secret so the test can sign with it. Uses the production
// CreateAccessKey path (returns the plaintext once).
func seedAccessKey(t *testing.T, svc *STSServiceImpl, accountID, userName string) (akid, secret string) {
	t.Helper()
	_, err := svc.iamSvc.CreateUser(accountID, &iam.CreateUserInput{UserName: aws.String(userName)})
	require.NoError(t, err)
	out, err := svc.iamSvc.CreateAccessKey(accountID, &iam.CreateAccessKeyInput{UserName: aws.String(userName)})
	require.NoError(t, err)
	return aws.StringValue(out.AccessKey.AccessKeyId), aws.StringValue(out.AccessKey.SecretAccessKey)
}

func withFrozenTime(t *testing.T, now time.Time) {
	t.Helper()
	prev := presignedTimeNow
	presignedTimeNow = func() time.Time { return now }
	t.Cleanup(func() { presignedTimeNow = prev })
}

// ----- Happy paths --------------------------------------------------------

func TestVerifyPresignedGetCallerIdentity_HappyPath_LongLived(t *testing.T) {
	svc, _ := newTestSetup(t)
	akid, secret := seedAccessKey(t, svc, testCallerAccountID, "alice")

	signedAt := time.Now().UTC().Truncate(time.Second)
	withFrozenTime(t, signedAt)

	const cluster = "demo-cluster"
	u := presignTestURL(t, akid, secret, cluster, signedAt, 900)

	got, err := svc.VerifyPresignedGetCallerIdentity(u, cluster)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, testCallerAccountID, got.AccountID)
	assert.Equal(t, cluster, got.XK8sAwsID)
	assert.Equal(t, principalTypeUserPresigned, got.PrincipalType)
	assert.Contains(t, got.ARN, ":user/alice")
}

func TestVerifyPresignedGetCallerIdentity_HappyPath_SessionCred(t *testing.T) {
	// A session credential (ASIA prefix) signs the URL. VerifyPresigned must
	// resolve via LookupSessionCredential, decrypt the secret, and match.
	svc, _ := newTestSetup(t)
	role := createRoleInAccount(t, svc, testCallerAccountID, "irsa-presign",
		trustPolicyAllowingUser(testCallerARN()))

	// Acquire a session credential the orthodox way.
	assumeOut, err := svc.AssumeRole(testCallerAccountID, testCallerARN(), testCallerUserName,
		basicAssumeRoleInput(*role.Arn, "sess-presign"))
	require.NoError(t, err)
	akid := aws.StringValue(assumeOut.Credentials.AccessKeyId)
	secret := aws.StringValue(assumeOut.Credentials.SecretAccessKey)

	signedAt := time.Now().UTC().Truncate(time.Second)
	withFrozenTime(t, signedAt)

	const cluster = "session-cluster"
	u := presignTestURL(t, akid, secret, cluster, signedAt, 900)

	got, err := svc.VerifyPresignedGetCallerIdentity(u, cluster)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, testCallerAccountID, got.AccountID)
	assert.Equal(t, cluster, got.XK8sAwsID)
	assert.Equal(t, principalTypeAssumedRolePresigned, got.PrincipalType)
}

// ----- Cross-cluster anti-replay (Q10 mandatory) --------------------------

func TestVerifyPresignedGetCallerIdentity_CrossClusterReplay(t *testing.T) {
	svc, _ := newTestSetup(t)
	akid, secret := seedAccessKey(t, svc, testCallerAccountID, "bob")
	signedAt := time.Now().UTC().Truncate(time.Second)
	withFrozenTime(t, signedAt)

	// Sign for cluster-A.
	u := presignTestURL(t, akid, secret, "cluster-A", signedAt, 900)

	// Present to cluster-B — signature reconstruction binds X-K8s-Aws-Id =
	// cluster-B, so the recomputed signature differs and verification fails.
	_, err := svc.VerifyPresignedGetCallerIdentity(u, "cluster-B")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidIdentityToken, err.Error())
}

func TestVerifyPresignedGetCallerIdentity_MissingXK8sAwsIdInSignedHeaders(t *testing.T) {
	// An attacker presigning without x-k8s-aws-id in SignedHeaders strips the
	// cross-cluster check from the canonical request. Verify must refuse
	// before any signature work — the URL is structurally untrustworthy.
	svc, _ := newTestSetup(t)
	akid, secret := seedAccessKey(t, svc, testCallerAccountID, "eve")

	signedAt := time.Now().UTC().Truncate(time.Second)
	withFrozenTime(t, signedAt)

	date := signedAt.UTC().Format("20060102")
	amzDate := signedAt.UTC().Format("20060102T150405Z")
	credentialScope := date + "/au-mel-1/sts/aws4_request"
	q := url.Values{}
	q.Set("Action", "GetCallerIdentity")
	q.Set("Version", "2011-06-15")
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", akid+"/"+credentialScope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", "900")
	q.Set("X-Amz-SignedHeaders", "host") // no x-k8s-aws-id

	canonQ := canonicalQueryStringForTest(q)
	canonHeaders := "host:" + testPresignedHost + "\n"
	canonReq := strings.Join([]string{
		"GET", "/", canonQ, canonHeaders, "host", emptyStringSHA256,
	}, "\n")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, credentialScope, hexSHA256(canonReq),
	}, "\n")
	key := deriveSigningKey(secret, date, "au-mel-1", "sts")
	sig := hex.EncodeToString(hmacSHA256(key, stringToSign))
	q.Set("X-Amz-Signature", sig)
	u := "https://" + testPresignedHost + "/?" + canonicalQueryStringForTest(q)

	_, err := svc.VerifyPresignedGetCallerIdentity(u, "any-cluster")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidIdentityToken, err.Error())
}

// ----- Expiry -------------------------------------------------------------

func TestVerifyPresignedGetCallerIdentity_Expired(t *testing.T) {
	svc, _ := newTestSetup(t)
	akid, secret := seedAccessKey(t, svc, testCallerAccountID, "carol")

	signedAt := time.Now().UTC().Truncate(time.Second)
	const cluster = "exp-cluster"
	u := presignTestURL(t, akid, secret, cluster, signedAt, 60)

	// Verifier clock 5 minutes after signing — beyond 60s expiry.
	withFrozenTime(t, signedAt.Add(5*time.Minute))
	_, err := svc.VerifyPresignedGetCallerIdentity(u, cluster)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorExpiredToken, err.Error())
}

// ----- Tampered URL detection ---------------------------------------------

func TestVerifyPresignedGetCallerIdentity_TamperedQueryParameter(t *testing.T) {
	// Flip an unrelated query param (Action) after signing — must invalidate
	// the signature because Action is part of the canonical query string.
	svc, _ := newTestSetup(t)
	akid, secret := seedAccessKey(t, svc, testCallerAccountID, "dave")
	signedAt := time.Now().UTC().Truncate(time.Second)
	withFrozenTime(t, signedAt)

	u := presignTestURL(t, akid, secret, "c", signedAt, 900)
	tampered := strings.Replace(u, "GetCallerIdentity", "GetSessionToken", 1)

	_, err := svc.VerifyPresignedGetCallerIdentity(tampered, "c")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidIdentityToken, err.Error())
}

// ----- Bad envelope / input edges ----------------------------------------

func TestVerifyPresignedGetCallerIdentity_MissingInput(t *testing.T) {
	svc, _ := newTestSetup(t)
	_, err := svc.VerifyPresignedGetCallerIdentity("", "c")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
	_, err = svc.VerifyPresignedGetCallerIdentity("https://x", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestVerifyPresignedGetCallerIdentity_RejectsHTTP(t *testing.T) {
	svc, _ := newTestSetup(t)
	_, err := svc.VerifyPresignedGetCallerIdentity("http://sts.local/?X-Amz-Algorithm=AWS4-HMAC-SHA256", "c")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidIdentityToken, err.Error())
}

func TestVerifyPresignedGetCallerIdentity_BadAlgorithm(t *testing.T) {
	svc, _ := newTestSetup(t)
	u := "https://sts.local/?X-Amz-Algorithm=foo&X-Amz-Credential=a/20240101/r/sts/aws4_request&X-Amz-Date=20240101T000000Z&X-Amz-Expires=900&X-Amz-SignedHeaders=host;x-k8s-aws-id&X-Amz-Signature=00"
	_, err := svc.VerifyPresignedGetCallerIdentity(u, "c")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidIdentityToken, err.Error())
}

func TestVerifyPresignedGetCallerIdentity_UnknownAccessKey(t *testing.T) {
	svc, _ := newTestSetup(t)
	signedAt := time.Now().UTC().Truncate(time.Second)
	withFrozenTime(t, signedAt)

	u := presignTestURL(t, "AKIAGHOSTEXAMPLE0000", "fake-secret", "c", signedAt, 900)
	_, err := svc.VerifyPresignedGetCallerIdentity(u, "c")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidIdentityToken, err.Error())
}

func TestVerifyPresignedGetCallerIdentity_UnknownAKIDPrefix(t *testing.T) {
	svc, _ := newTestSetup(t)
	signedAt := time.Now().UTC().Truncate(time.Second)
	withFrozenTime(t, signedAt)
	u := presignTestURL(t, "QQQQEXAMPLE000000000", "x", "c", signedAt, 900)
	_, err := svc.VerifyPresignedGetCallerIdentity(u, "c")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidIdentityToken, err.Error())
}

func TestVerifyPresignedGetCallerIdentity_BadServiceInCredential(t *testing.T) {
	svc, _ := newTestSetup(t)
	akid, secret := seedAccessKey(t, svc, testCallerAccountID, "frank")

	signedAt := time.Now().UTC().Truncate(time.Second)
	withFrozenTime(t, signedAt)

	// Build a URL whose credential scope claims s3 instead of sts — the
	// service mismatch is structural, no signature work needed.
	date := signedAt.UTC().Format("20060102")
	amzDate := signedAt.UTC().Format("20060102T150405Z")
	q := url.Values{}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", akid+"/"+date+"/au-mel-1/s3/aws4_request")
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", "900")
	q.Set("X-Amz-SignedHeaders", "host;x-k8s-aws-id")
	q.Set("X-Amz-Signature", strings.Repeat("a", 64))
	_ = secret
	u := "https://" + testPresignedHost + "/?" + q.Encode()
	_, err := svc.VerifyPresignedGetCallerIdentity(u, "c")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidIdentityToken, err.Error())
}

func TestVerifyPresignedGetCallerIdentity_CredentialDateMismatch(t *testing.T) {
	// X-Amz-Date date portion (YYYYMMDD) and credential-scope date must agree.
	// AWS-CLI enforces this; mismatch is a tampered URL.
	svc, _ := newTestSetup(t)
	akid, secret := seedAccessKey(t, svc, testCallerAccountID, "george")
	signedAt := time.Now().UTC().Truncate(time.Second)
	withFrozenTime(t, signedAt)

	q := url.Values{}
	wrongDate := signedAt.Add(-24 * time.Hour).Format("20060102")
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", akid+"/"+wrongDate+"/au-mel-1/sts/aws4_request")
	q.Set("X-Amz-Date", signedAt.Format("20060102T150405Z"))
	q.Set("X-Amz-Expires", "900")
	q.Set("X-Amz-SignedHeaders", "host;x-k8s-aws-id")
	q.Set("X-Amz-Signature", strings.Repeat("a", 64))
	_ = secret
	u := "https://" + testPresignedHost + "/?" + q.Encode()
	_, err := svc.VerifyPresignedGetCallerIdentity(u, "c")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidIdentityToken, err.Error())
}

// ----- Component unit tests -----------------------------------------------

func TestAWSQueryEscape(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"abc", "abc"},
		{"a b", "a%20b"},
		{"a+b", "a%2Bb"},
		{"a/b", "a%2Fb"},
		{"a=b", "a%3Db"},
		{"~_.-", "~_.-"}, // unreserved set passes through
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, awsQueryEscape(tc.in))
		})
	}
}

func TestDeriveSigningKey_KnownVector(t *testing.T) {
	// From AWS SigV4 docs (Example signing key derivation).
	// Inputs:
	//   secret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	//   date   = "20120215", region = "us-east-1", service = "iam"
	// The doc's expected signing-key hex digest stays stable across SDK versions.
	key := deriveSigningKey(
		"wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		"20120215", "us-east-1", "iam",
	)
	const wantHex = "f4780e2d9f65fa895f9c67b32ce1baf0b0d8a43505a000a1a9e090d414db404d"
	assert.Equal(t, wantHex, hex.EncodeToString(key))
}

// TestVerifyPresignedGetCallerIdentity_RealSDKPresign signs the URL with the
// actual aws-sdk-go v4 presigner — the same canonicalisation `aws eks
// get-token` uses — rather than the hand-rolled presignTestURL. This is the
// faithful end-to-end guard: it caught the payload-hash bug (verify used the
// S3-only "UNSIGNED-PAYLOAD" instead of the empty-string SHA256 that botocore
// signs non-S3 requests with), which every hand-rolled test masked by using
// the same wrong constant.
func TestVerifyPresignedGetCallerIdentity_RealSDKPresign(t *testing.T) {
	svc, _ := newTestSetup(t)
	akid, secret := seedAccessKey(t, svc, testCallerAccountID, "carol")

	signedAt := time.Now().UTC().Truncate(time.Second)
	withFrozenTime(t, signedAt)

	const cluster = "prod-cluster"
	req, err := http.NewRequest(http.MethodGet,
		"https://"+testPresignedHost+"/?Action=GetCallerIdentity&Version=2011-06-15", nil)
	require.NoError(t, err)
	req.Header.Set("X-K8s-Aws-Id", cluster)

	signer := v4.NewSigner(credentials.NewStaticCredentials(akid, secret, ""))
	_, err = signer.Presign(req, nil, "sts", testPresignedRegion, 900*time.Second, signedAt)
	require.NoError(t, err)

	got, err := svc.VerifyPresignedGetCallerIdentity(req.URL.String(), cluster)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, cluster, got.XK8sAwsID)
	assert.Contains(t, got.ARN, ":user/carol")

	// Anti-replay: the SDK-signed URL must NOT verify against a different cluster.
	_, err = svc.VerifyPresignedGetCallerIdentity(req.URL.String(), "other-cluster")
	require.Error(t, err)
}
