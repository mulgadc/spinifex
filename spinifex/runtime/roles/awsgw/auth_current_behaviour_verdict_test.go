package awsgw_test

//test:in-package — this characterization drives unexported gateway wiring.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	ekscluster "github.com/mulgadc/spinifex/spinifex/domains/eks/cluster"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/pipeline"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/accountadmission"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/credverify"
)

// Pins CURRENT behaviour: wire outcomes for invalid long-lived credentials, and
// that key status changes apply to the very next request (no credential cache).
func TestCurrentBehaviour_Auth_LongLivedRejections(t *testing.T) {
	e := newCBEnv(t, cbEnvOpts{})
	gci := cbForm("GetCallerIdentity")

	t.Run("unknown_key", func(t *testing.T) {
		got := cbOutcomeOf(e.request(t, e.h, cbCreds{akid: cbUnknownKey, secret: cbBadSecret}, "sts", gci))
		assert.Equal(t, cbRegistry(awserrors.ErrorInvalidClientTokenId), got)
	})

	t.Run("wrong_secret", func(t *testing.T) {
		got := cbOutcomeOf(e.request(t, e.h, cbCreds{akid: e.alice.akid, secret: cbBadSecret}, "sts", gci))
		assert.Equal(t, cbRegistry(awserrors.ErrorSignatureDoesNotMatch), got)
	})

	t.Run("malformed_authorization_header", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(gci.Encode()))
		req.RemoteAddr = "10.200.0.1:40000"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=garbage")
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		assert.Equal(t, cbRegistry(awserrors.ErrorIncompleteSignature), cbOutcomeOf(rec))
	})

	t.Run("key_made_inactive_after_use", func(t *testing.T) {
		creds := e.newKey(t, "alice")
		require.Equal(t, http.StatusOK, e.callerIdentity(t, creds).Code)
		_, err := e.iam.UpdateAccessKey(e.accountID, &iam.UpdateAccessKeyInput{
			UserName: aws.String("alice"), AccessKeyId: aws.String(creds.akid), Status: aws.String(iamdomain.AccessKeyStatusInactive),
		})
		require.NoError(t, err)
		assert.Equal(t, cbRegistry(awserrors.ErrorInvalidClientTokenId), cbOutcomeOf(e.callerIdentity(t, creds)))
	})

	t.Run("key_deleted_after_use", func(t *testing.T) {
		u, _ := e.newUser(t, "dave")
		require.Equal(t, http.StatusOK, e.callerIdentity(t, u).Code)
		_, err := e.iam.DeleteAccessKey(e.accountID, &iam.DeleteAccessKeyInput{
			UserName: aws.String("dave"), AccessKeyId: aws.String(u.akid),
		})
		require.NoError(t, err)
		assert.Equal(t, cbRegistry(awserrors.ErrorInvalidClientTokenId), cbOutcomeOf(e.callerIdentity(t, u)))
	})
}

// Pins CURRENT behaviour: AssumeRole session credentials at the SigV4 door.
func TestCurrentBehaviour_Session_AssumeRoleCredentials(t *testing.T) {
	e := newCBEnv(t, cbEnvOpts{})
	roleARN, roleID := e.createAssumableRole(t, "app")
	s := e.assumeRole(t, roleARN, "s1")
	other := e.assumeRole(t, roleARN, "s2")

	t.Run("valid_session", func(t *testing.T) {
		rec := e.callerIdentity(t, s)
		require.Equalf(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		body := rec.Body.String()
		assert.Equal(t, "arn:aws:sts::"+e.accountID+":assumed-role/app/s1", cbXML(body, "Arn"))
		assert.Equal(t, roleID+":s1", cbXML(body, "UserId"))
		assert.Equal(t, e.accountID, cbXML(body, "Account"))
	})

	invalid := cbRegistry(awserrors.ErrorInvalidClientTokenId)
	for name, c := range map[string]cbCreds{
		"missing_token":        {akid: s.akid, secret: s.secret},
		"other_sessions_token": {akid: s.akid, secret: s.secret, token: other.token},
		"tampered_token":       {akid: s.akid, secret: s.secret, token: tamper(s.token)},
		"unknown_session_akid": {akid: cbUnknownSes, secret: s.secret, token: s.token},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, invalid, cbOutcomeOf(e.callerIdentity(t, c)))
		})
	}

	// CURRENT: an AKIA key ignores X-Amz-Security-Token entirely; the header is
	// only signed, never checked, so a long-lived key plus any token succeeds.
	t.Run("long_lived_with_session_token", func(t *testing.T) {
		rec := e.callerIdentity(t, cbCreds{akid: e.alice.akid, secret: e.alice.secret, token: s.token})
		require.Equalf(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		assert.Equal(t, "arn:aws:iam::"+e.accountID+":user/alice", cbXML(rec.Body.String(), "Arn"))
	})

	t.Run("valid_token_wrong_secret", func(t *testing.T) {
		got := cbOutcomeOf(e.callerIdentity(t, cbCreds{akid: s.akid, secret: cbBadSecret, token: s.token}))
		assert.Equal(t, cbRegistry(awserrors.ErrorSignatureDoesNotMatch), got)
	})
}

// Pins CURRENT behaviour: an expired session answers ExpiredToken (HTTP 400)
// before its session token is examined.
func TestCurrentBehaviour_Session_Expired(t *testing.T) {
	e := newCBEnv(t, cbEnvOpts{})
	roleARN, _ := e.createAssumableRole(t, "app")
	s := e.assumeRole(t, roleARN, "s1")
	require.Equal(t, http.StatusOK, e.callerIdentity(t, s).Code)
	e.expireSession(t, s.akid)

	// CURRENT: ExpiredToken is 400 here; AWS STS/IAM answer it as 403 (unverified).
	expired := cbRegistry(awserrors.ErrorExpiredToken)
	require.Equal(t, http.StatusBadRequest, expired.status)
	for name, c := range map[string]cbCreds{
		"valid_token": s,
		// CURRENT: expiry is checked ahead of the token, so a caller holding only
		// the cleartext ASIA key ID learns the session has expired.
		"no_token":    {akid: s.akid, secret: s.secret},
		"wrong_token": {akid: s.akid, secret: cbBadSecret, token: tamper(s.token)},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, expired, cbOutcomeOf(e.callerIdentity(t, c)))
		})
	}
}

// Pins CURRENT behaviour: a GetSessionToken session resolves to the IAM user and
// outlives the long-lived key that minted it.
func TestCurrentBehaviour_Session_GetSessionTokenOutlivesMintingKey(t *testing.T) {
	e := newCBEnv(t, cbEnvOpts{})
	mintKey := e.newKey(t, "alice")
	s := e.getSessionToken(t, mintKey)

	rec := e.callerIdentity(t, s)
	require.Equalf(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "arn:aws:iam::"+e.accountID+":user/alice", cbXML(rec.Body.String(), "Arn"))
	assert.Equal(t, e.aliceID, cbXML(rec.Body.String(), "UserId"))

	_, err := e.iam.UpdateAccessKey(e.accountID, &iam.UpdateAccessKeyInput{
		UserName: aws.String("alice"), AccessKeyId: aws.String(mintKey.akid), Status: aws.String(iamdomain.AccessKeyStatusInactive),
	})
	require.NoError(t, err)
	_, err = e.iam.DeleteAccessKey(e.accountID, &iam.DeleteAccessKeyInput{
		UserName: aws.String("alice"), AccessKeyId: aws.String(mintKey.akid),
	})
	require.NoError(t, err)

	// CURRENT: the session record carries no reference to the minting key, so
	// deactivating and deleting that key leaves the session authenticating.
	assert.Equal(t, http.StatusOK, e.callerIdentity(t, s).Code)

	// A session cannot mint a further session.
	assert.Equal(t, cbRegistry(awserrors.ErrorAccessDenied).code,
		cbOutcomeOf(e.request(t, e.h, s, "sts", cbForm("GetSessionToken"))).code)
}

// Pins CURRENT behaviour: an AssumeRoleWithWebIdentity session authenticates at
// the SigV4 door exactly as an AssumeRole session does.
func TestCurrentBehaviour_Session_WebIdentityCredentials(t *testing.T) {
	e := newCBEnv(t, cbEnvOpts{})
	issuer := testDiscIssuer

	iamKV, err := kvutil.GetOrCreateBucket(t.Context(), e.js,
		iamdomain.IAMAccountBucketName(e.accountID), iamdomain.KVBucketIAMAccountVersion)
	require.NoError(t, err)
	_, err = iamKV.Put(t.Context(), iamdomain.OIDCProviderKey(issuer), []byte(`{"registered":true}`))
	require.NoError(t, err)

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	point, err := priv.PublicKey.Bytes()
	require.NoError(t, err)
	jwks, err := json.Marshal(stsdomain.JWKS{Keys: []stsdomain.JWK{{
		Kty: "EC", Kid: "k1", Alg: "ES256", Use: "sig", Crv: "P-256",
		X: base64.RawURLEncoding.EncodeToString(point[1:33]),
		Y: base64.RawURLEncoding.EncodeToString(point[33:65]),
	}}})
	require.NoError(t, err)
	eksKV, err := ekscluster.GetOrCreateAccountBucket(t.Context(), e.js, e.accountID)
	require.NoError(t, err)
	_, err = eksKV.Put(t.Context(), ekscluster.OIDCJWKSKey(testDiscCluster), jwks)
	require.NoError(t, err)

	federated := iamdomain.OIDCProviderARN(e.accountID, strings.TrimPrefix(issuer, "https://"))
	role, err := e.iam.CreateRole(e.accountID, &iam.CreateRoleInput{
		RoleName: aws.String("irsa"),
		AssumeRolePolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Federated":"` +
			federated + `"},"Action":"sts:AssumeRoleWithWebIdentity"}]}`),
	})
	require.NoError(t, err)

	now := time.Now().UTC()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss": issuer, "sub": "system:serviceaccount:default:sa", "aud": []string{"sts.amazonaws.com"},
		"exp": now.Add(10 * time.Minute).Unix(), "iat": now.Unix(),
	})
	tok.Header["kid"] = "k1"
	signed, err := tok.SignedString(priv)
	require.NoError(t, err)

	// Unsigned: the anonymous STS interceptor serves it ahead of SigV4.
	s := cbSessionFrom(t, e.request(t, e.h, cbCreds{}, "sts", cbForm("AssumeRoleWithWebIdentity",
		"RoleArn", aws.StringValue(role.Role.Arn), "RoleSessionName", "pod", "WebIdentityToken", signed)))

	rec := e.callerIdentity(t, s)
	require.Equalf(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "arn:aws:sts::"+e.accountID+":assumed-role/irsa/pod", cbXML(rec.Body.String(), "Arn"))
	assert.Equal(t, cbRegistry(awserrors.ErrorInvalidClientTokenId),
		cbOutcomeOf(e.callerIdentity(t, cbCreds{akid: s.akid, secret: s.secret, token: tamper(s.token)})))
}

// Pins CURRENT behaviour: deleting a user and recreating it under the same name
// strands its old key and sessions, by revocation or by the continuity check.
func TestCurrentBehaviour_Revocation_UserDeletedAndRecreated(t *testing.T) {
	for _, tc := range []struct {
		name        string
		noRevoker   bool
		storedAfter bool
	}{
		{name: "revoker_installed", noRevoker: false, storedAfter: false},
		{name: "revocation_missed", noRevoker: true, storedAfter: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newCBEnv(t, cbEnvOpts{noRevoker: tc.noRevoker})
			oldKey, oldID := e.newUser(t, "bob")
			session := e.getSessionToken(t, oldKey)
			require.Equal(t, http.StatusOK, e.callerIdentity(t, session).Code)

			_, err := e.iam.DeleteAccessKey(e.accountID, &iam.DeleteAccessKeyInput{
				UserName: aws.String("bob"), AccessKeyId: aws.String(oldKey.akid),
			})
			require.NoError(t, err)
			_, err = e.iam.DeleteUser(e.accountID, &iam.DeleteUserInput{UserName: aws.String("bob")})
			require.NoError(t, err)
			assert.Equal(t, tc.storedAfter, e.sessionStored(t, session.akid))

			invalid := cbRegistry(awserrors.ErrorInvalidClientTokenId)
			assert.Equal(t, invalid, cbOutcomeOf(e.callerIdentity(t, session)), "session after delete")

			newKey, newID := e.newUser(t, "bob")
			require.NotEqual(t, oldID, newID)
			assert.Equal(t, invalid, cbOutcomeOf(e.callerIdentity(t, oldKey)), "old key after recreate")
			assert.Equal(t, invalid, cbOutcomeOf(e.callerIdentity(t, session)), "old session after recreate")

			rec := e.callerIdentity(t, newKey)
			require.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, newID, cbXML(rec.Body.String(), "UserId"))
		})
	}
}

// Pins CURRENT behaviour: deleting a role and recreating it under the same name
// strands its old sessions, by revocation or by the continuity check.
func TestCurrentBehaviour_Revocation_RoleDeletedAndRecreated(t *testing.T) {
	for _, tc := range []struct {
		name        string
		noRevoker   bool
		storedAfter bool
	}{
		{name: "revoker_installed", noRevoker: false, storedAfter: false},
		{name: "revocation_missed", noRevoker: true, storedAfter: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newCBEnv(t, cbEnvOpts{noRevoker: tc.noRevoker})
			roleARN, oldRoleID := e.createAssumableRole(t, "app")
			s := e.assumeRole(t, roleARN, "s1")
			require.Equal(t, http.StatusOK, e.callerIdentity(t, s).Code)

			_, err := e.iam.DeleteRole(e.accountID, &iam.DeleteRoleInput{RoleName: aws.String("app")})
			require.NoError(t, err)
			assert.Equal(t, tc.storedAfter, e.sessionStored(t, s.akid))

			invalid := cbRegistry(awserrors.ErrorInvalidClientTokenId)
			assert.Equal(t, invalid, cbOutcomeOf(e.callerIdentity(t, s)), "session after delete")

			newARN, newRoleID := e.createAssumableRole(t, "app")
			require.Equal(t, roleARN, newARN)
			require.NotEqual(t, oldRoleID, newRoleID)
			assert.Equal(t, invalid, cbOutcomeOf(e.callerIdentity(t, s)), "old session after recreate")
			assert.Equal(t, http.StatusOK, e.callerIdentity(t, e.assumeRole(t, newARN, "s2")).Code)
		})
	}
}

// Pins CURRENT behaviour: whose identity policy the policy check applies to an
// orphaned key once its user is deleted and recreated under the same name.
func TestCurrentBehaviour_Revocation_OrphanedKeyPermissionsAfterRecreation(t *testing.T) {
	e := newCBEnv(t, cbEnvOpts{})
	key, oldID := e.newUser(t, "frank")
	listUsers := cbForm("ListUsers")
	getUser := cbForm("GetUser", "UserName", "frank")

	// Policy A: ListUsers only.
	e.putUserPolicy(t, "frank", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"iam:ListUsers","Resource":"*"}]}`)
	require.Equal(t, http.StatusOK, e.request(t, e.h, key, "iam", listUsers).Code, "A allows ListUsers")
	require.Equal(t, awserrors.ErrorAccessDenied, cbOutcomeOf(e.request(t, e.h, key, "iam", getUser)).code, "A denies GetUser")

	e.orphanKeys(t, "frank")
	_, err := e.iam.DeleteUserPolicy(e.accountID, &iam.DeleteUserPolicyInput{UserName: aws.String("frank"), PolicyName: aws.String("p")})
	require.NoError(t, err)
	_, err = e.iam.DeleteUser(e.accountID, &iam.DeleteUserInput{UserName: aws.String("frank")})
	require.NoError(t, err)

	// Policy B, on the replacement user: GetUser only.
	u, err := e.iam.CreateUser(e.accountID, &iam.CreateUserInput{UserName: aws.String("frank")})
	require.NoError(t, err)
	require.NotEqual(t, oldID, aws.StringValue(u.User.UserId))
	e.putUserPolicy(t, "frank", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"iam:GetUser","Resource":"*"}]}`)

	// CURRENT: the orphaned key is evaluated against the replacement user's
	// policy B, so it gains GetUser and loses the ListUsers that A granted.
	rec := e.request(t, e.h, key, "iam", getUser)
	assert.Equalf(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, awserrors.ErrorAccessDenied, cbOutcomeOf(e.request(t, e.h, key, "iam", listUsers)).code)
}

// Pins CURRENT behaviour: unsigned requests are refused with
// MissingAuthenticationToken unless the action is anonymous STS.
func TestCurrentBehaviour_Auth_MissingCredentials(t *testing.T) {
	e := newCBEnv(t, cbEnvOpts{})
	missing := cbRegistry(awserrors.ErrorMissingAuthenticationToken)
	for _, f := range []url.Values{
		cbForm("ListUsers"),
		cbForm("GetCallerIdentity"),
		cbForm("GetSessionToken"),
		cbForm("AssumeRole", "RoleArn", "arn:aws:iam::"+e.accountID+":role/x", "RoleSessionName", "s"),
	} {
		t.Run(f.Get("Action"), func(t *testing.T) {
			assert.Equal(t, missing, cbOutcomeOf(e.request(t, e.h, cbCreds{}, "", f)))
		})
	}

	// Session token alone, with no Authorization header.
	t.Run("token_without_signature", func(t *testing.T) {
		assert.Equal(t, missing, cbOutcomeOf(e.request(t, e.h, cbCreds{token: "anything"}, "", cbForm("GetCallerIdentity"))))
	})

	// The anonymous path reaches STS itself: its verdict, not the door's.
	t.Run("AssumeRoleWithWebIdentity", func(t *testing.T) {
		got := cbOutcomeOf(e.request(t, e.h, cbCreds{}, "", cbForm("AssumeRoleWithWebIdentity",
			"RoleArn", "arn:aws:iam::"+e.accountID+":role/x", "RoleSessionName", "s1", "WebIdentityToken", "not-a-jwt")))
		assert.Equal(t, http.StatusBadRequest, got.status)
		assert.Equal(t, awserrors.ErrorInvalidIdentityToken, got.code)
	})
}

// Pins CURRENT behaviour: which credential failures a caller can tell apart on
// the wire, and whether the door decrypted a secret before answering.
func TestCurrentBehaviour_AntiEnumeration_RejectionShapes(t *testing.T) {
	e := newCBEnv(t, cbEnvOpts{})
	bobKey, _ := e.newUser(t, "bob")
	_, err := e.iam.UpdateAccessKey(e.accountID, &iam.UpdateAccessKeyInput{
		UserName: aws.String("bob"), AccessKeyId: aws.String(bobKey.akid), Status: aws.String(iamdomain.AccessKeyStatusInactive),
	})
	require.NoError(t, err)
	carolKey, _ := e.newUser(t, "carol")
	_, err = e.iam.DeleteAccessKey(e.accountID, &iam.DeleteAccessKeyInput{UserName: aws.String("carol"), AccessKeyId: aws.String(carolKey.akid)})
	require.NoError(t, err)
	live := e.getSessionToken(t, e.alice)
	expired := e.getSessionToken(t, e.alice)
	e.expireSession(t, expired.akid)

	invalid := cbRegistry(awserrors.ErrorInvalidClientTokenId)
	sigMismatch := cbRegistry(awserrors.ErrorSignatureDoesNotMatch)
	for _, tc := range []struct {
		name     string
		creds    cbCreds
		want     cbOutcome
		decrypts int32
	}{
		{"unknown_key", cbCreds{akid: cbUnknownKey, secret: cbBadSecret}, invalid, 0},
		{"unknown_prefix", cbCreds{akid: "AKIBCBUNKNOWNPREFIXA", secret: cbBadSecret}, invalid, 0},
		{"deleted_key_right_secret", carolKey, invalid, 0},
		{"inactive_key_right_secret", bobKey, invalid, 0},
		// CURRENT: an inactive key answers InvalidClientTokenId without a
		// signature check, while an active key with a bad signature answers
		// SignatureDoesNotMatch, so a caller with no secret can tell the two apart.
		{"inactive_key_bad_signature", cbCreds{akid: bobKey.akid, secret: cbBadSecret}, invalid, 0},
		{"active_key_bad_signature", cbCreds{akid: e.alice.akid, secret: cbBadSecret}, sigMismatch, 1},
		{"unknown_session", cbCreds{akid: cbUnknownSes, secret: cbBadSecret, token: live.token}, invalid, 0},
		{"session_no_token", cbCreds{akid: live.akid, secret: cbBadSecret}, invalid, 0},
		{"session_wrong_token", cbCreds{akid: live.akid, secret: cbBadSecret, token: tamper(live.token)}, invalid, 0},
		{"session_expired_no_token", cbCreds{akid: expired.akid, secret: cbBadSecret}, cbRegistry(awserrors.ErrorExpiredToken), 0},
		{"session_right_token_bad_signature", cbCreds{akid: live.akid, secret: cbBadSecret, token: live.token}, sigMismatch, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e.counter.decrypts.Store(0)
			assert.Equal(t, tc.want, cbOutcomeOf(e.callerIdentity(t, tc.creds)))
			assert.Equal(t, tc.decrypts, e.counter.decrypts.Load(), "secret decrypts before the verdict")
		})
	}
}

// Pins CURRENT behaviour: the door keeps no account-status cache. Every request reads
// the account through the in-process IAM service, so a status written out of band is
// seen by the next request; keys and sessions are likewise re-read on every request.
func TestCurrentBehaviour_NoAccountStatusCache(t *testing.T) {
	denied := cbOutcome{status: http.StatusForbidden, code: awserrors.ErrorAccessDenied,
		message: awserrors.ErrorLookup[awserrors.ErrorAccessDenied].Message}

	for _, status := range []string{iamdomain.AccountStatusSuspended, iamdomain.AccountStatusTerminating} {
		t.Run(status, func(t *testing.T) {
			e := newCBEnv(t, cbEnvOpts{})
			gw := gwfix.WithDeps(&pipeline.Config{
				DisableLogging: true,
				Region:         gwfix.Region,
				Credentials:    credverify.New(e.counter, e.sts),
				Admission:      accountadmission.New(e.counter),
			}, gwfix.Deps{NATS: e.nc, Nodes: 1, IAM: e.counter, STS: e.sts})
			withCapabilities(gw)
			withIAMAndSTS(gw)
			h := gw.SetupRoutes()

			require.Equal(t, http.StatusOK, e.request(t, h, e.alice, "sts", cbForm("GetCallerIdentity")).Code)
			_, err := e.iam.SetAccountStatus(e.accountID, status)
			require.NoError(t, err)

			assert.Equal(t, denied, cbOutcomeOf(e.request(t, h, e.alice, "sts", cbForm("GetCallerIdentity"))),
				"the request after the write must be refused, with no wait")
		})
	}
}
