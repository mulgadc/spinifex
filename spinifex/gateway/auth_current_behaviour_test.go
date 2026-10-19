package gateway

//test:in-package — this characterization drives unexported gateway wiring.

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/golang-jwt/jwt/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/internal/testkit"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	handlers_sts "github.com/mulgadc/spinifex/spinifex/handlers/sts"
)

// The TestCurrentBehaviour_ tests in this file pin today's SigV4 authentication,
// STS session, revocation, backing-store and identity-policy outcomes at the
// AWSGW door. They drive the real router over real IAM and STS services on an
// embedded JetStream, and describe current behaviour, not the target design.

const (
	cbRootAKID   = "AKIACBROOTKEYAAAAAAA"
	cbRootSecret = "cb-root-secret-0123456789abcdefghijklmn"
	cbUnknownKey = "AKIACBUNKNOWNKEYAAAA"
	cbUnknownSes = "ASIACBUNKNOWNSESSION"
	cbBadSecret  = "WRONG-secret-0123456789abcdefghijklmnop"
)

type cbCreds struct {
	akid, secret, token string
}

// cbDecryptCounter is the real IAM service with DecryptSecret counted, so a test
// can observe whether the door decrypted a secret before rejecting a request.
type cbDecryptCounter struct {
	*handlers_iam.IAMServiceImpl

	decrypts atomic.Int32
}

func (c *cbDecryptCounter) DecryptSecret(ciphertext string) (string, error) {
	c.decrypts.Add(1)
	return c.IAMServiceImpl.DecryptSecret(ciphertext)
}

type cbEnvOpts struct {
	// noRevoker leaves IAM without the STS session revoker, standing in for a
	// revocation that failed at delete time.
	noRevoker bool
	// separateIAMConn gives IAM and STS their own NATS connection, so it can be
	// closed while the gateway's own connection stays up.
	separateIAMConn bool
}

type cbEnv struct {
	nc        *nats.Conn // the gateway's connection
	iamNC     *nats.Conn // IAM/STS connection; == nc unless separateIAMConn
	js        jetstream.JetStream
	iam       *handlers_iam.IAMServiceImpl
	sts       *handlers_sts.STSServiceImpl
	counter   *cbDecryptCounter
	h         http.Handler
	accountID string
	alice     cbCreds
	aliceID   string
	addr      atomic.Uint32
}

func newCBEnv(t *testing.T, opts cbEnvOpts) *cbEnv {
	t.Helper()
	srv, nc, _ := testutil.StartTestJetStream(t)
	e := &cbEnv{nc: nc, iamNC: nc, js: testutil.NewJetStream(t, nc)}
	if opts.separateIAMConn {
		iamNC, err := nats.Connect(srv.ClientURL())
		require.NoError(t, err)
		t.Cleanup(iamNC.Close)
		e.iamNC = iamNC
	}

	masterKey, err := handlers_iam.GenerateMasterKey()
	require.NoError(t, err)
	e.iam, err = handlers_iam.NewIAMServiceImpl(t.Context(), e.iamNC, masterKey)
	require.NoError(t, err)
	iamJS := testutil.NewJetStream(t, e.iamNC)
	e.sts, err = handlers_sts.NewSTSServiceImpl(t.Context(), e.iamNC, e.iam, masterKey, handlers_eks.NewOIDCJWKSReader(iamJS))
	require.NoError(t, err)
	if !opts.noRevoker {
		e.iam.SetSessionRevoker(e.sts)
	}

	encrypted, err := handlers_iam.EncryptSecret(cbRootSecret, masterKey)
	require.NoError(t, err)
	require.NoError(t, e.iam.SeedBootstrap(&handlers_iam.BootstrapData{
		AccessKeyID: cbRootAKID, EncryptedSecret: encrypted, AccountID: awsidentifiers.GlobalAccountID,
	}))

	acct, err := e.iam.CreateAccount("tenant")
	require.NoError(t, err)
	require.Equal(t, testDiscAccount, acct.AccountID, "web-identity fixtures assume the first tenant account")
	e.accountID = acct.AccountID
	e.alice, e.aliceID = e.newUser(t, "alice")

	e.counter = &cbDecryptCounter{IAMServiceImpl: e.iam}
	e.h = e.gateway()
	return e
}

// gateway builds a router over the env's services with its own (cold) caches.
func (e *cbEnv) gateway() http.Handler {
	gw := &GatewayConfig{
		DisableLogging: true,
		NATSConn:       e.nc,
		Region:         testRegion,
		ExpectedNodes:  1,
		IAMService:     e.counter,
		STSService:     e.sts,
	}
	return gw.SetupRoutes()
}

func (e *cbEnv) newUser(t *testing.T, name string) (cbCreds, string) {
	t.Helper()
	u, err := e.iam.CreateUser(e.accountID, &iam.CreateUserInput{UserName: aws.String(name)})
	require.NoError(t, err)
	return e.newKey(t, name), aws.StringValue(u.User.UserId)
}

func (e *cbEnv) newKey(t *testing.T, user string) cbCreds {
	t.Helper()
	k, err := e.iam.CreateAccessKey(e.accountID, &iam.CreateAccessKeyInput{UserName: aws.String(user)})
	require.NoError(t, err)
	return cbCreds{akid: aws.StringValue(k.AccessKey.AccessKeyId), secret: aws.StringValue(k.AccessKey.SecretAccessKey)}
}

func (e *cbEnv) putUserPolicy(t *testing.T, user, doc string) {
	t.Helper()
	_, err := e.iam.PutUserPolicy(e.accountID, &iam.PutUserPolicyInput{
		UserName: aws.String(user), PolicyName: aws.String("p"), PolicyDocument: aws.String(doc),
	})
	require.NoError(t, err)
}

// request builds a signed (or, for a zero cbCreds, unsigned) query-protocol POST.
// Each request gets a distinct source address so the auth-failure lockout,
// which is per source IP, never couples one case to another.
func (e *cbEnv) request(t *testing.T, h http.Handler, c cbCreds, service string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	body := []byte(form.Encode())
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Host = "localhost:9999"
	n := e.addr.Add(1)
	req.RemoteAddr = fmt.Sprintf("10.%d.%d.%d:40000", (n>>16)&0xff, (n>>8)&0xff, n&0xff)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if c.token != "" {
		req.Header.Set("X-Amz-Security-Token", c.token)
	}
	if c.akid != "" {
		signTestRequestAs(t, req, body, c.akid, c.secret, service, time.Now().UTC())
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func (e *cbEnv) callerIdentity(t *testing.T, c cbCreds) *httptest.ResponseRecorder {
	t.Helper()
	return e.request(t, e.h, c, "sts", cbForm("GetCallerIdentity"))
}

// cbIAMActions are the IAM actions these tests send; every other action is STS.
var cbIAMActions = map[string]bool{"ListUsers": true, "CreateUser": true, "GetUser": true}

func cbForm(action string, kv ...string) url.Values {
	v := url.Values{"Action": {action}, "Version": {"2011-06-15"}}
	if cbIAMActions[action] {
		v.Set("Version", "2010-05-08")
	}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

func cbXML(body, tag string) string {
	m := regexp.MustCompile(`<` + tag + `>([^<]*)</` + tag + `>`).FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return m[1]
}

// cbOutcome is the wire verdict a client sees: status, AWS error code, message.
type cbOutcome struct {
	status  int
	code    string
	message string
}

func cbOutcomeOf(rec *httptest.ResponseRecorder) cbOutcome {
	body := rec.Body.String()
	return cbOutcome{status: rec.Code, code: cbXML(body, "Code"), message: cbXML(body, "Message")}
}

func cbRegistry(code string) cbOutcome {
	m := awserrors.ErrorLookup[code]
	return cbOutcome{status: m.HTTPCode, code: code, message: m.Message}
}

func cbSessionFrom(t *testing.T, rec *httptest.ResponseRecorder) cbCreds {
	t.Helper()
	require.Equalf(t, http.StatusOK, rec.Code, "mint failed: %s", rec.Body.String())
	body := rec.Body.String()
	c := cbCreds{akid: cbXML(body, "AccessKeyId"), secret: cbXML(body, "SecretAccessKey"), token: cbXML(body, "SessionToken")}
	require.True(t, strings.HasPrefix(c.akid, "ASIA"), "session AKID %q", c.akid)
	require.NotEmpty(t, c.token)
	return c
}

// createAssumableRole creates role name trusting alice, and grants alice
// sts:AssumeRole on it.
func (e *cbEnv) createAssumableRole(t *testing.T, name string) (roleARN, roleID string) {
	t.Helper()
	trust := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::` +
		e.accountID + `:user/alice"},"Action":"sts:AssumeRole"}]}`
	out, err := e.iam.CreateRole(e.accountID, &iam.CreateRoleInput{
		RoleName: aws.String(name), AssumeRolePolicyDocument: aws.String(trust),
	})
	require.NoError(t, err)
	roleARN = aws.StringValue(out.Role.Arn)
	e.putUserPolicy(t, "alice", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"sts:AssumeRole","Resource":"*"}]}`)
	return roleARN, aws.StringValue(out.Role.RoleId)
}

func (e *cbEnv) assumeRole(t *testing.T, roleARN, session string) cbCreds {
	t.Helper()
	return cbSessionFrom(t, e.request(t, e.h, e.alice, "sts",
		cbForm("AssumeRole", "RoleArn", roleARN, "RoleSessionName", session)))
}

func (e *cbEnv) getSessionToken(t *testing.T, c cbCreds) cbCreds {
	t.Helper()
	return cbSessionFrom(t, e.request(t, e.h, c, "sts", cbForm("GetSessionToken")))
}

// expireSession rewrites a stored session record's expiry into the past.
func (e *cbEnv) expireSession(t *testing.T, akid string) {
	t.Helper()
	kv, err := e.js.KeyValue(t.Context(), handlers_sts.KVBucketSessionCredentials)
	require.NoError(t, err)
	entry, err := kv.Get(t.Context(), akid)
	require.NoError(t, err)
	var cred handlers_sts.SessionCredential
	require.NoError(t, json.Unmarshal(entry.Value(), &cred))
	cred.ExpiresAt = time.Now().UTC().Add(-time.Minute)
	data, err := json.Marshal(cred)
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), akid, data)
	require.NoError(t, err)
}

func (e *cbEnv) sessionStored(t *testing.T, akid string) bool {
	t.Helper()
	cred, err := e.sts.LookupSessionCredential(akid)
	require.NoError(t, err)
	return cred != nil
}

func tamper(s string) string {
	b := []byte(s)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b)
}

// Pins CURRENT behaviour: how a valid long-lived key resolves to a principal and account.
func TestCurrentBehaviour_Auth_LongLivedKeyResolvesPrincipal(t *testing.T) {
	e := newCBEnv(t, cbEnvOpts{})

	t.Run("iam_user", func(t *testing.T) {
		rec := e.callerIdentity(t, e.alice)
		require.Equalf(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		body := rec.Body.String()
		assert.Equal(t, "arn:aws:iam::"+e.accountID+":user/alice", cbXML(body, "Arn"))
		assert.Equal(t, e.aliceID, cbXML(body, "UserId"))
		assert.Equal(t, e.accountID, cbXML(body, "Account"))
	})

	t.Run("global_root", func(t *testing.T) {
		rec := e.callerIdentity(t, cbCreds{akid: cbRootAKID, secret: cbRootSecret})
		require.Equalf(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		body := rec.Body.String()
		assert.Equal(t, "arn:aws:iam::"+awsidentifiers.GlobalAccountID+":root", cbXML(body, "Arn"))
		assert.Equal(t, awsidentifiers.GlobalAccountID, cbXML(body, "UserId"))
		assert.Equal(t, awsidentifiers.GlobalAccountID, cbXML(body, "Account"))
	})

	// CURRENT: a tenant IAM user literally named "root" is resolved with the
	// account ID as its UserId (aws:userid), the value AWS reserves for the
	// account root, rather than the AIDA ID stored on its own user record.
	t.Run("tenant_user_named_root", func(t *testing.T) {
		creds, storedID := e.newUser(t, "root")
		require.True(t, strings.HasPrefix(storedID, "AIDA"))
		rec := e.callerIdentity(t, creds)
		require.Equalf(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		body := rec.Body.String()
		assert.Equal(t, "arn:aws:iam::"+e.accountID+":user/root", cbXML(body, "Arn"))
		assert.Equal(t, e.accountID, cbXML(body, "UserId"))
		assert.NotEqual(t, storedID, cbXML(body, "UserId"))
	})
}

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
			UserName: aws.String("alice"), AccessKeyId: aws.String(creds.akid), Status: aws.String(handlers_iam.AccessKeyStatusInactive),
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
		UserName: aws.String("alice"), AccessKeyId: aws.String(mintKey.akid), Status: aws.String(handlers_iam.AccessKeyStatusInactive),
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
		handlers_iam.IAMAccountBucketName(e.accountID), handlers_iam.KVBucketIAMAccountVersion)
	require.NoError(t, err)
	_, err = iamKV.Put(t.Context(), handlers_iam.OIDCProviderKey(issuer), []byte(`{"registered":true}`))
	require.NoError(t, err)

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	point, err := priv.PublicKey.Bytes()
	require.NoError(t, err)
	jwks, err := json.Marshal(handlers_sts.JWKS{Keys: []handlers_sts.JWK{{
		Kty: "EC", Kid: "k1", Alg: "ES256", Use: "sig", Crv: "P-256",
		X: base64.RawURLEncoding.EncodeToString(point[1:33]),
		Y: base64.RawURLEncoding.EncodeToString(point[33:65]),
	}}})
	require.NoError(t, err)
	eksKV, err := handlers_eks.GetOrCreateAccountBucket(t.Context(), e.js, e.accountID)
	require.NoError(t, err)
	_, err = eksKV.Put(t.Context(), handlers_eks.OIDCJWKSKey(testDiscCluster), jwks)
	require.NoError(t, err)

	federated := handlers_iam.OIDCProviderARN(e.accountID, strings.TrimPrefix(issuer, "https://"))
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

// Pins CURRENT behaviour: a long-lived key is bound to a user by name only, so
// a key record orphaned from its user authenticates as a same-name replacement.
func TestCurrentBehaviour_Revocation_OrphanedKeyAuthenticatesAsRecreatedUser(t *testing.T) {
	e := newCBEnv(t, cbEnvOpts{})
	key, oldID := e.newUser(t, "erin")

	e.orphanKeys(t, "erin")

	// CURRENT: the orphaned key still authenticates, and nothing blocks DeleteUser.
	require.Equal(t, http.StatusOK, e.callerIdentity(t, key).Code)
	_, err := e.iam.DeleteUser(e.accountID, &iam.DeleteUserInput{UserName: aws.String("erin")})
	require.NoError(t, err)

	// CURRENT: with the user gone, the key authenticates and GetCallerIdentity
	// fails on the missing record rather than at the door.
	gone := cbOutcomeOf(e.callerIdentity(t, key))
	assert.Equal(t, http.StatusNotFound, gone.status)
	assert.Equal(t, awserrors.ErrorIAMNoSuchEntity, gone.code)

	// CURRENT: a new user with the same name inherits the orphaned key; AWS binds
	// keys to the user's unique ID, so a replacement would not (unverified).
	_, err = e.iam.CreateUser(e.accountID, &iam.CreateUserInput{UserName: aws.String("erin")})
	require.NoError(t, err)
	rec := e.callerIdentity(t, key)
	require.Equalf(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.NotEqual(t, oldID, cbXML(rec.Body.String(), "UserId"))
	assert.Equal(t, "arn:aws:iam::"+e.accountID+":user/erin", cbXML(rec.Body.String(), "Arn"))
}

// orphanKeys rewrites a user record to list no access keys while leaving the
// access-key records in place, the state an interrupted DeleteAccessKey leaves.
func (e *cbEnv) orphanKeys(t *testing.T, user string) {
	t.Helper()
	usersKV, err := e.js.KeyValue(t.Context(), handlers_iam.KVBucketUsers)
	require.NoError(t, err)
	entry, err := usersKV.Get(t.Context(), e.accountID+"."+user)
	require.NoError(t, err)
	var u handlers_iam.User
	require.NoError(t, json.Unmarshal(entry.Value(), &u))
	u.AccessKeys = []string{}
	data, err := json.Marshal(u)
	require.NoError(t, err)
	_, err = usersKV.Put(t.Context(), e.accountID+"."+user, data)
	require.NoError(t, err)
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

// Pins CURRENT behaviour: credential lookup outcomes when IAM or STS backing
// state cannot be read.
func TestCurrentBehaviour_Auth_BackingStateUnavailable(t *testing.T) {
	gci := cbForm("GetCallerIdentity")
	internal := cbRegistry(awserrors.ErrorInternalError)

	t.Run("gateway_connection_closed", func(t *testing.T) {
		e := newCBEnv(t, cbEnvOpts{})
		roleARN, _ := e.createAssumableRole(t, "app")
		s := e.assumeRole(t, roleARN, "s1")
		e.nc.Close()
		want := cbOutcome{status: http.StatusServiceUnavailable, code: awserrors.ErrorServiceUnavailable, message: clusterUnavailableMsg}
		for name, c := range map[string]cbCreds{"long_lived": e.alice, "session": s, "unknown": {akid: cbUnknownKey, secret: cbBadSecret}} {
			assert.Equal(t, want, cbOutcomeOf(e.request(t, e.h, c, "sts", gci)), name)
		}
	})

	t.Run("iam_connection_closed", func(t *testing.T) {
		e := newCBEnv(t, cbEnvOpts{separateIAMConn: true})
		roleARN, _ := e.createAssumableRole(t, "app")
		s := e.assumeRole(t, roleARN, "s1")
		e.iamNC.Close()
		for name, c := range map[string]cbCreds{"long_lived": e.alice, "session": s, "unknown": {akid: cbUnknownKey, secret: cbBadSecret}} {
			assert.Equal(t, internal, cbOutcomeOf(e.request(t, e.h, c, "sts", gci)), name)
		}
	})

	t.Run("access_key_bucket_deleted", func(t *testing.T) {
		e := newCBEnv(t, cbEnvOpts{})
		require.NoError(t, e.js.DeleteKeyValue(t.Context(), handlers_iam.KVBucketAccessKeys))
		for name, c := range map[string]cbCreds{"known": e.alice, "unknown": {akid: cbUnknownKey, secret: cbBadSecret}} {
			assert.Equal(t, internal, cbOutcomeOf(e.request(t, e.h, c, "sts", gci)), name)
		}
	})

	t.Run("session_bucket_deleted", func(t *testing.T) {
		e := newCBEnv(t, cbEnvOpts{})
		roleARN, _ := e.createAssumableRole(t, "app")
		s := e.assumeRole(t, roleARN, "s1")
		require.NoError(t, e.js.DeleteKeyValue(t.Context(), handlers_sts.KVBucketSessionCredentials))
		assert.Equal(t, internal, cbOutcomeOf(e.callerIdentity(t, s)))
	})

	t.Run("accounts_bucket_deleted", func(t *testing.T) {
		e := newCBEnv(t, cbEnvOpts{})
		require.NoError(t, e.js.DeleteKeyValue(t.Context(), handlers_iam.KVBucketAccounts))
		// Fails closed only once the signature holds; a bad one still reads as such.
		assert.Equal(t, internal, cbOutcomeOf(e.callerIdentity(t, e.alice)))
		assert.Equal(t, cbRegistry(awserrors.ErrorSignatureDoesNotMatch),
			cbOutcomeOf(e.callerIdentity(t, cbCreds{akid: e.alice.akid, secret: cbBadSecret})))
	})
}

// Pins CURRENT behaviour: identity-policy allow, implicit deny and explicit deny
// on iam actions, evaluated after authentication and before the handler runs.
func TestCurrentBehaviour_Policy_IdentityPolicyOutcomes(t *testing.T) {
	e := newCBEnv(t, cbEnvOpts{})
	listUsers := cbForm("ListUsers")
	denied := cbOutcome{status: http.StatusForbidden, code: awserrors.ErrorAccessDenied,
		message: awserrors.LookupErrorMessage("iam", awserrors.ErrorAccessDenied).Message}

	t.Run("implicit_deny", func(t *testing.T) {
		assert.Equal(t, denied, cbOutcomeOf(e.request(t, e.h, e.alice, "iam", listUsers)))
	})

	t.Run("allow", func(t *testing.T) {
		e.putUserPolicy(t, "alice", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"iam:ListUsers","Resource":"*"}]}`)
		rec := e.request(t, e.h, e.alice, "iam", listUsers)
		require.Equalf(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		assert.Contains(t, rec.Body.String(), "<UserName>alice</UserName>")
	})

	t.Run("explicit_deny_blocks_before_dispatch", func(t *testing.T) {
		e.putUserPolicy(t, "alice", `{"Version":"2012-10-17","Statement":[`+
			`{"Effect":"Allow","Action":"iam:*","Resource":"*"},`+
			`{"Effect":"Deny","Action":"iam:CreateUser","Resource":"*"}]}`)
		assert.Equal(t, denied, cbOutcomeOf(e.request(t, e.h, e.alice, "iam", cbForm("CreateUser", "UserName", "mallory"))))
		_, err := e.iam.GetUser(e.accountID, &iam.GetUserInput{UserName: aws.String("mallory")})
		require.Error(t, err)
		assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity, "denied CreateUser must not have run")
		assert.Equal(t, http.StatusOK, e.request(t, e.h, e.alice, "iam", listUsers).Code)
	})

	// CURRENT: the root-name policy bypass is limited to the global account, so a
	// tenant user named "root" with no policy is implicitly denied.
	t.Run("tenant_user_named_root_implicit_deny", func(t *testing.T) {
		creds, _ := e.newUser(t, "root")
		assert.Equal(t, denied, cbOutcomeOf(e.request(t, e.h, creds, "iam", listUsers)))
	})
}

// Pins CURRENT behaviour: which credential failures a caller can tell apart on
// the wire, and whether the door decrypted a secret before answering.
func TestCurrentBehaviour_AntiEnumeration_RejectionShapes(t *testing.T) {
	e := newCBEnv(t, cbEnvOpts{})
	bobKey, _ := e.newUser(t, "bob")
	_, err := e.iam.UpdateAccessKey(e.accountID, &iam.UpdateAccessKeyInput{
		UserName: aws.String("bob"), AccessKeyId: aws.String(bobKey.akid), Status: aws.String(handlers_iam.AccessKeyStatusInactive),
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

// Pins CURRENT behaviour: the door caches only an account's ACTIVE status, per
// gateway, for accountActiveTTL; keys and sessions are re-read on every request.
func TestCurrentBehaviour_Cache_AccountStatusIsTheOnlyAuthCache(t *testing.T) {
	require.Equal(t, 5*time.Second, accountActiveTTL)

	for _, status := range []string{handlers_iam.AccountStatusSuspended, handlers_iam.AccountStatusTerminating} {
		t.Run(status, func(t *testing.T) {
			e := newCBEnv(t, cbEnvOpts{})
			require.Equal(t, http.StatusOK, e.callerIdentity(t, e.alice).Code)
			start := time.Now()
			_, err := e.iam.SetAccountStatus(e.accountID, status)
			require.NoError(t, err)

			// CURRENT: an out-of-band status write is not seen by a gateway
			// holding a warm ACTIVE entry until that entry expires.
			warm := e.callerIdentity(t, e.alice)
			if time.Since(start) >= accountActiveTTL {
				t.Skip("cache window elapsed before the warm request; outcome not attributable")
			}
			assert.Equal(t, http.StatusOK, warm.Code)

			denied := cbOutcome{status: http.StatusForbidden, code: awserrors.ErrorAccessDenied,
				message: awserrors.ErrorLookup[awserrors.ErrorAccessDenied].Message}
			assert.Equal(t, denied, cbOutcomeOf(e.request(t, e.gateway(), e.alice, "sts", cbForm("GetCallerIdentity"))))
		})
	}
}
