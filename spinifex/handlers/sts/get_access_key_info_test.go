package handlers_sts

import (
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lookupStubIAMService records every LookupAccessKey call and returns a
// canned result, so a test can assert both the outcome and whether the KV
// bucket was reached at all.
type lookupStubIAMService struct {
	handlers_iam.IAMService

	calls int
	key   *handlers_iam.AccessKey
	err   error
}

func (s *lookupStubIAMService) LookupAccessKey(string) (*handlers_iam.AccessKey, error) {
	s.calls++
	return s.key, s.err
}

func TestGetAccessKeyInfo_ResolvesOwningAccount(t *testing.T) {
	svc, _ := newTestSetup(t)
	akid, _ := seedAccessKey(t, svc, testCallerAccountID, "alice")

	out, err := svc.GetAccessKeyInfo(&sts.GetAccessKeyInfoInput{AccessKeyId: aws.String(akid)})
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, testCallerAccountID, aws.StringValue(out.Account))
}

// AWS does not scope this call to the caller, so a key owned by another account
// resolves normally rather than reporting not-found.
func TestGetAccessKeyInfo_ResolvesKeyFromAnotherAccount(t *testing.T) {
	svc, _ := newTestSetup(t)
	const otherAccount = "111122223333"
	akid, _ := seedAccessKey(t, svc, otherAccount, "bob")

	out, err := svc.GetAccessKeyInfo(&sts.GetAccessKeyInfoInput{AccessKeyId: aws.String(akid)})
	require.NoError(t, err)
	assert.Equal(t, otherAccount, aws.StringValue(out.Account))
}

// An ASIA key lives in the session bucket, which the IAM lookup never sees. It
// must resolve through the fallback instead of coming back as unknown.
func TestGetAccessKeyInfo_ResolvesSessionCredential(t *testing.T) {
	svc, _ := newTestSetup(t)
	seedUser(t, svc, testCallerAccountID, "carol")

	tok, err := svc.GetSessionToken(testCallerAccountID, "carol", principalTypeUser, "AKIA0123456789ABCDEF", &sts.GetSessionTokenInput{})
	require.NoError(t, err)
	sessionAKID := aws.StringValue(tok.Credentials.AccessKeyId)
	require.Contains(t, sessionAKID, SessionAccessKeyIDPrefix)

	out, err := svc.GetAccessKeyInfo(&sts.GetAccessKeyInfoInput{AccessKeyId: aws.String(sessionAKID)})
	require.NoError(t, err)
	assert.Equal(t, testCallerAccountID, aws.StringValue(out.Account))
}

func TestGetAccessKeyInfo_UnknownKeyIsValidationError(t *testing.T) {
	svc, _ := newTestSetup(t)

	cases := []struct {
		name string
		akid string
	}{
		{"unknown long-lived", "AKIANOTAREALKEY00000"},
		{"unknown session", SessionAccessKeyIDPrefix + "NOTAREALKEY00000"},
		{"unknown prefix", "AROANOTAREALKEY00000"},
		{"underscore", "AKIA_0123456789ABCD"},
		{"lowercase", "akiaiosfodnn7example"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := svc.GetAccessKeyInfo(&sts.GetAccessKeyInfoInput{AccessKeyId: aws.String(tc.akid)})
			require.Error(t, err)
			assert.Nil(t, out)
			code, message, ok := awserrors.ResolveErrorDetail(err)
			require.True(t, ok)
			assert.Equal(t, awserrors.ErrorValidationError, code)
			assert.Equal(t, "Access key ID is not valid.", message)
		})
	}
}

// A value that cannot name a stored key is refused on shape, before any KV read.
func TestGetAccessKeyInfo_MalformedKeyRejectedWithoutLookup(t *testing.T) {
	svc, _ := newTestSetup(t)
	stub := &lookupStubIAMService{err: errors.New(awserrors.ErrorIAMNoSuchEntity)}
	svc.iamSvc = stub

	cases := []struct {
		name string
		akid string
		want string
	}{
		{"too short", "AKIA0123",
			"1 validation error detected: Value 'AKIA0123' at 'accessKeyId' failed to satisfy constraint: Member must have length greater than or equal to 16"},
		{"too long", strings.Repeat("A", 129),
			"1 validation error detected: Value '" + strings.Repeat("A", 129) + "' at 'accessKeyId' failed to satisfy constraint: Member must have length less than or equal to 128"},
		{"punctuation", "AKIA-0123456789ABCD",
			"1 validation error detected: Value 'AKIA-0123456789ABCD' at 'accessKeyId' failed to satisfy constraint: Member must satisfy regular expression pattern: [\\w]*"},
		{"whitespace", "AKIA 0123456789ABCD",
			"1 validation error detected: Value 'AKIA 0123456789ABCD' at 'accessKeyId' failed to satisfy constraint: Member must satisfy regular expression pattern: [\\w]*"},
		{"too short and punctuation", "AKIA-012",
			"2 validation errors detected: Value 'AKIA-012' at 'accessKeyId' failed to satisfy constraint: Member must satisfy regular expression pattern: [\\w]*; " +
				"Value 'AKIA-012' at 'accessKeyId' failed to satisfy constraint: Member must have length greater than or equal to 16"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := svc.GetAccessKeyInfo(&sts.GetAccessKeyInfoInput{AccessKeyId: aws.String(tc.akid)})
			assert.Nil(t, out)
			requireAWSError(t, err, awserrors.ErrorValidationError, tc.want)
		})
	}

	_, err := svc.GetAccessKeyInfo(&sts.GetAccessKeyInfoInput{AccessKeyId: aws.String("")})
	requireAWSError(t, err, awserrors.ErrorMissingParameter, "")
	assert.Zero(t, stub.calls, "malformed access key ID must not reach the KV bucket")
}

func TestGetAccessKeyInfo_NilInputIsMissingParameter(t *testing.T) {
	svc, _ := newTestSetup(t)
	_, err := svc.GetAccessKeyInfo(nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

// A dependency outage must not collapse into not-found, or every key would be
// reported as nonexistent for the duration of the outage.
func TestGetAccessKeyInfo_KVFaultIsInternalError(t *testing.T) {
	svc, _ := newTestSetup(t)
	svc.iamSvc = &lookupStubIAMService{err: errors.New("nats: connection closed")}

	out, err := svc.GetAccessKeyInfo(&sts.GetAccessKeyInfoInput{AccessKeyId: aws.String("AKIA0123456789ABCDEF")})
	require.Error(t, err)
	assert.Nil(t, out)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
}

// A stored record with no account is a corrupt record, not an answer: reporting
// a blank account would read to a caller as a mismatch with their own.
func TestGetAccessKeyInfo_RecordWithoutAccountIsInternalError(t *testing.T) {
	svc, _ := newTestSetup(t)
	svc.iamSvc = &lookupStubIAMService{key: &handlers_iam.AccessKey{AccessKeyID: "AKIA0123456789ABCDEF"}}

	_, err := svc.GetAccessKeyInfo(&sts.GetAccessKeyInfoInput{AccessKeyId: aws.String("AKIA0123456789ABCDEF")})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
}
