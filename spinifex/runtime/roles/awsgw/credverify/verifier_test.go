package credverify

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testAKID    = "AKIATESTCONFORMANCE00"
	testSession = "ASIATESTCONFORMANCE00"
	testAccount = "000000000001"
)

// fakeIAM embeds the interface so a method the verifier should not call panics.
type fakeIAM struct {
	iamdomain.IAMService

	key       *iamdomain.AccessKey
	lookupErr error
	getUserFn func(accountID string, input *iam.GetUserInput) (*iam.GetUserOutput, error)
	getUsers  atomic.Int32
	decrypts  atomic.Int32
}

func (f *fakeIAM) LookupAccessKey(string) (*iamdomain.AccessKey, error) {
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	if f.key == nil {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return f.key, nil
}

func (f *fakeIAM) DecryptSecret(string) (string, error) {
	f.decrypts.Add(1)
	return "secret", nil
}

func (f *fakeIAM) GetUser(accountID string, input *iam.GetUserInput) (*iam.GetUserOutput, error) {
	f.getUsers.Add(1)
	if f.getUserFn != nil {
		return f.getUserFn(accountID, input)
	}
	name := aws.StringValue(input.UserName)
	return &iam.GetUserOutput{User: &iam.User{UserName: aws.String(name), UserId: aws.String("AIDA" + name)}}, nil
}

type fakeSTS struct {
	stsdomain.STSService

	cred         *stsdomain.SessionCredential
	tokenOK      bool
	principalErr error
	principals   atomic.Int32
}

func (f *fakeSTS) LookupSessionCredential(string) (*stsdomain.SessionCredential, error) {
	return f.cred, nil
}

func (f *fakeSTS) VerifySessionToken(*stsdomain.SessionCredential, string) bool { return f.tokenOK }

func (f *fakeSTS) VerifySessionPrincipal(*stsdomain.SessionCredential) (*stsdomain.SessionPrincipal, error) {
	f.principals.Add(1)
	return nil, f.principalErr
}

func accept(string) error { return nil }

func longLived(iamSvc *fakeIAM, user string) *fakeIAM {
	iamSvc.key = &iamdomain.AccessKey{
		AccessKeyID: testAKID, UserName: user, AccountID: testAccount, Status: iamdomain.AccessKeyStatusActive,
	}
	return iamSvc
}

func resolveLongLived(t *testing.T, iamSvc *fakeIAM, user string) (credential.Principal, *credential.Failure) {
	t.Helper()
	v := New(longLived(iamSvc, user), nil)
	req := credential.Request{AccessKeyID: testAKID}
	auth, f := v.Authenticate(req, accept)
	require.Nil(t, f)
	return v.Resolve(req, auth)
}

func roleSession(assumedRoleID string) *stsdomain.SessionCredential {
	return &stsdomain.SessionCredential{
		AccessKeyID: testSession, AccountID: testAccount, SessionName: "session",
		PrincipalType: stsdomain.PrincipalTypeAssumedRole, AssumedRoleID: assumedRoleID,
		AssumedRoleARN:    "arn:aws:sts::000000000001:assumed-role/SharedOps/session",
		UnderlyingRoleARN: "arn:aws:iam::000000000001:role/SharedOps",
		ExpiresAt:         time.Now().Add(time.Hour),
	}
}

func resolveRoleSession(t *testing.T, assumedRoleID string) (credential.Principal, *credential.Failure) {
	t.Helper()
	v := New(&fakeIAM{}, &fakeSTS{cred: roleSession(assumedRoleID), tokenOK: true})
	req := credential.Request{AccessKeyID: testSession, SecurityToken: "tok"}
	auth, f := v.Authenticate(req, accept)
	require.Nil(t, f)
	return v.Resolve(req, auth)
}

// aws:userid is resolved once, where the principal is. A user with no record and
// a session minted before the ID was recorded both yield an empty ID, not a failure.
func TestResolve_UserIDResolvesAndOmits(t *testing.T) {
	p, f := resolveLongLived(t, &fakeIAM{}, "alice")
	require.Nil(t, f)
	assert.Equal(t, "AIDAalice", p.UserID)

	p, f = resolveRoleSession(t, "AROASHAREDOPS:session")
	require.Nil(t, f)
	assert.Equal(t, "AROASHAREDOPS:session", p.UserID)

	// Root's aws:userid is the account ID, matching GetCallerIdentity.
	p, f = resolveLongLived(t, &fakeIAM{}, "root")
	require.Nil(t, f)
	assert.Equal(t, testAccount, p.UserID)

	p, f = resolveLongLived(t, &fakeIAM{getUserFn: func(string, *iam.GetUserInput) (*iam.GetUserOutput, error) {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}}, "alice")
	require.Nil(t, f, "a deleted user omits the ID rather than failing the request")
	assert.Empty(t, p.UserID)

	// A record predating the field resolves to no ID.
	p, f = resolveLongLived(t, &fakeIAM{getUserFn: func(string, *iam.GetUserInput) (*iam.GetUserOutput, error) {
		return &iam.GetUserOutput{User: &iam.User{UserName: aws.String("alice")}}, nil
	}}, "alice")
	require.Nil(t, f)
	assert.Empty(t, p.UserID)

	// A session minted before assumed_role_id was recorded takes the same arm.
	p, f = resolveRoleSession(t, "")
	require.Nil(t, f)
	assert.Empty(t, p.UserID)
}

// A dependency fault must not read as "the principal has no ID": the key would be
// omitted, silently narrowing an Allow and widening a Deny to everything.
func TestResolve_DependencyFaultFailsClosed(t *testing.T) {
	p, f := resolveLongLived(t, &fakeIAM{getUserFn: func(string, *iam.GetUserInput) (*iam.GetUserOutput, error) {
		return nil, errors.New("nats: no responders available for request")
	}}, "alice")

	require.NotNil(t, f)
	assert.Equal(t, awserrors.ErrorInternalError, f.Code)
	assert.Empty(t, f.FingerprintKind, "a fault is not a guess at a credential")
	assert.Equal(t, credential.Principal{}, p)
}

// An IAM user's ARN comes from its record, path included, in the same read as its
// ID. Root and role sessions carry no user ARN.
func TestResolve_StoredUserARN(t *testing.T) {
	const pathed = "arn:aws:iam::000000000001:user/eng/alice"
	withARN := func() *fakeIAM {
		return &fakeIAM{getUserFn: func(string, *iam.GetUserInput) (*iam.GetUserOutput, error) {
			return &iam.GetUserOutput{User: &iam.User{
				UserName: aws.String("alice"), UserId: aws.String("AIDAALICE"), Arn: aws.String(pathed),
			}}, nil
		}}
	}

	p, f := resolveLongLived(t, withARN(), "alice")
	require.Nil(t, f)
	assert.Equal(t, "AIDAALICE", p.UserID)
	assert.Equal(t, pathed, p.UserARN)

	p, f = resolveLongLived(t, withARN(), "root")
	require.Nil(t, f)
	assert.Empty(t, p.UserARN)

	p, f = resolveRoleSession(t, "AROASHAREDOPS:session")
	require.Nil(t, f)
	assert.Empty(t, p.UserARN)
}

// Nothing past the secret lookup runs before the signature verifies: an AKID travels
// in cleartext, so a prober holding one must not be able to drive IAM or STS reads.
func TestAuthenticate_NoReadsBeforeSignatureVerifies(t *testing.T) {
	reject := func(string) error { return errors.New("bad signature") }

	iamSvc := longLived(&fakeIAM{}, "alice")
	_, f := New(iamSvc, nil).Authenticate(credential.Request{AccessKeyID: testAKID}, reject)
	require.NotNil(t, f)
	assert.True(t, f.SignatureMismatch)
	assert.Equal(t, awserrors.ErrorSignatureDoesNotMatch, f.Code)
	assert.Zero(t, iamSvc.getUsers.Load(), "no user read before the signature verifies")

	sts := &fakeSTS{cred: roleSession("AROA:session"), tokenOK: true}
	_, f = New(&fakeIAM{}, sts).Authenticate(credential.Request{AccessKeyID: testSession, SecurityToken: "tok"}, reject)
	require.NotNil(t, f)
	assert.True(t, f.SignatureMismatch)
	assert.Zero(t, sts.principals.Load(), "no continuity check before the signature verifies")
}

// The secret reaches only the verifySig callback.
func TestAuthenticate_SecretOnlyReachesCallback(t *testing.T) {
	var seen string
	auth, f := New(longLived(&fakeIAM{}, "alice"), nil).Authenticate(credential.Request{AccessKeyID: testAKID},
		func(secret string) error { seen = secret; return nil })
	require.Nil(t, f)
	assert.Equal(t, "secret", seen)
	assert.Equal(t, testAccount, auth.AccountID)
}

func TestAuthenticate_Rejections(t *testing.T) {
	expired := roleSession("AROA:session")
	expired.ExpiresAt = time.Now().Add(-time.Minute)

	cases := []struct {
		name string
		v    *Verifier
		req  credential.Request
		code string
		kind string
	}{
		{"unknown prefix", New(&fakeIAM{}, nil), credential.Request{AccessKeyID: "ZZIAX"},
			awserrors.ErrorInvalidClientTokenId, "unknown-prefix"},
		{"key not found", New(&fakeIAM{}, nil), credential.Request{AccessKeyID: testAKID},
			awserrors.ErrorInvalidClientTokenId, "akid-not-found"},
		{"key inactive", New(&fakeIAM{key: &iamdomain.AccessKey{Status: "Inactive"}}, nil),
			credential.Request{AccessKeyID: testAKID}, awserrors.ErrorInvalidClientTokenId, "akid-inactive"},
		{"iam fault", New(&fakeIAM{lookupErr: errors.New("boom")}, nil), credential.Request{AccessKeyID: testAKID},
			awserrors.ErrorInternalError, ""},
		{"no iam", New(nil, nil), credential.Request{AccessKeyID: testAKID}, awserrors.ErrorInternalError, ""},
		{"session without sts", New(&fakeIAM{}, nil), credential.Request{AccessKeyID: testSession},
			awserrors.ErrorInternalError, ""},
		{"session not found", New(&fakeIAM{}, &fakeSTS{}), credential.Request{AccessKeyID: testSession},
			awserrors.ErrorInvalidClientTokenId, "session-not-found"},
		{"session expired", New(&fakeIAM{}, &fakeSTS{cred: expired}), credential.Request{AccessKeyID: testSession},
			awserrors.ErrorExpiredToken, "session-expired"},
		{"session without token", New(&fakeIAM{}, &fakeSTS{cred: roleSession("A")}), credential.Request{AccessKeyID: testSession},
			awserrors.ErrorInvalidClientTokenId, "session-no-token"},
		{"session token mismatch", New(&fakeIAM{}, &fakeSTS{cred: roleSession("A")}),
			credential.Request{AccessKeyID: testSession, SecurityToken: "tok"},
			awserrors.ErrorInvalidClientTokenId, "session-token-mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			_, f := tc.v.Authenticate(tc.req, func(string) error { called = true; return nil })
			require.NotNil(t, f)
			assert.Equal(t, tc.code, f.Code)
			assert.Equal(t, tc.kind, f.FingerprintKind)
			assert.False(t, called, "a rejected credential never reaches signature verification")
		})
	}
}

// A token-mismatch fingerprint carries a digest of the token, never the token.
func TestAuthenticate_TokenMismatchFingerprintHoldsNoBearerToken(t *testing.T) {
	_, f := New(&fakeIAM{}, &fakeSTS{cred: roleSession("A")}).Authenticate(
		credential.Request{AccessKeyID: testSession, SecurityToken: "bearer-token-value"}, accept)
	require.NotNil(t, f)
	assert.NotContains(t, f.FingerprintParts, "bearer-token-value")
}

// Gone and Replaced count toward the lockout; Legacy must not, or every record
// predating the check would lock out a shared egress address on deploy.
func TestResolve_SessionPrincipalVerdicts(t *testing.T) {
	cases := []struct {
		name string
		err  error
		kind string
	}{
		{"gone", stsdomain.ErrSessionPrincipalGone, "session-principal"},
		{"replaced", stsdomain.ErrSessionPrincipalReplaced, "session-principal"},
		{"legacy", stsdomain.ErrSessionPrincipalLegacy, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := New(&fakeIAM{}, &fakeSTS{cred: roleSession("A"), tokenOK: true, principalErr: tc.err})
			req := credential.Request{AccessKeyID: testSession, SecurityToken: "tok"}
			auth, f := v.Authenticate(req, accept)
			require.Nil(t, f)

			_, f = v.Resolve(req, auth)
			require.NotNil(t, f)
			assert.Equal(t, awserrors.ErrorInvalidClientTokenId, f.Code)
			assert.Equal(t, tc.kind, f.FingerprintKind)
		})
	}

	t.Run("fault", func(t *testing.T) {
		v := New(&fakeIAM{}, &fakeSTS{cred: roleSession("A"), tokenOK: true, principalErr: errors.New("kv down")})
		req := credential.Request{AccessKeyID: testSession, SecurityToken: "tok"}
		auth, f := v.Authenticate(req, accept)
		require.Nil(t, f)
		_, f = v.Resolve(req, auth)
		require.NotNil(t, f)
		assert.Equal(t, awserrors.ErrorInternalError, f.Code)
		assert.Empty(t, f.FingerprintKind)
	})
}

// A long-lived credential has no session record, so the continuity check is skipped.
func TestResolve_LongLivedSkipsContinuityCheck(t *testing.T) {
	sts := &fakeSTS{}
	v := New(longLived(&fakeIAM{}, "alice"), sts)
	req := credential.Request{AccessKeyID: testAKID}
	auth, f := v.Authenticate(req, accept)
	require.Nil(t, f)
	_, f = v.Resolve(req, auth)
	require.Nil(t, f)
	assert.Zero(t, sts.principals.Load())
}

func TestResolve_RejectsForeignHandle(t *testing.T) {
	_, f := New(&fakeIAM{}, nil).Resolve(credential.Request{}, credential.Authenticated{Handle: "not ours"})
	require.NotNil(t, f)
	assert.Equal(t, awserrors.ErrorInternalError, f.Code)
}
