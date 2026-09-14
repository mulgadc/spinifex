package handlers_sts

import (
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
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

func TestGetAccessKeyInfo_UnknownKeyIsInvalidClientToken(t *testing.T) {
	svc, _ := newTestSetup(t)

	cases := []struct {
		name string
		akid string
	}{
		{"unknown long-lived", "AKIANOTAREALKEY00000"},
		{"unknown session", SessionAccessKeyIDPrefix + "NOTAREALKEY00000"},
		{"unknown prefix", "AROANOTAREALKEY00000"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := svc.GetAccessKeyInfo(&sts.GetAccessKeyInfoInput{AccessKeyId: aws.String(tc.akid)})
			require.Error(t, err)
			assert.Nil(t, out)
			assert.Equal(t, awserrors.ErrorInvalidClientTokenId, err.Error())
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
		{"empty", "", awserrors.ErrorMissingParameter},
		{"too short", "AKIA0123", awserrors.ErrorValidationError},
		{"too long", strings.Repeat("A", maxAccessKeyIDLength+1), awserrors.ErrorValidationError},
		{"punctuation", "AKIA-0123456789ABCD", awserrors.ErrorValidationError},
		{"whitespace", "AKIA 0123456789ABCD", awserrors.ErrorValidationError},
		{"arn not a key id", "arn:aws:iam::000000000000:user/alice", awserrors.ErrorValidationError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := svc.GetAccessKeyInfo(&sts.GetAccessKeyInfoInput{AccessKeyId: aws.String(tc.akid)})
			require.Error(t, err)
			assert.Nil(t, out)
			assert.Equal(t, tc.want, err.Error())
		})
	}
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

func TestIsAccessKeyIDShaped(t *testing.T) {
	assert.True(t, isAccessKeyIDShaped("AKIA0123456789ABCDEF"))
	assert.True(t, isAccessKeyIDShaped(strings.Repeat("A", minAccessKeyIDLength)))
	assert.True(t, isAccessKeyIDShaped(strings.Repeat("A", maxAccessKeyIDLength)))
	assert.False(t, isAccessKeyIDShaped(strings.Repeat("A", minAccessKeyIDLength-1)))
	assert.False(t, isAccessKeyIDShaped(strings.Repeat("A", maxAccessKeyIDLength+1)))
	assert.False(t, isAccessKeyIDShaped("AKIA_123456789ABCDE"))
}
