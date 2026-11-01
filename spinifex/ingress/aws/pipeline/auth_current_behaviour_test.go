package pipeline

//test:in-package — this characterization drives unexported gateway wiring.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
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
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/internal/testkit"
	ekscluster "github.com/mulgadc/spinifex/spinifex/domains/eks/cluster"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
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
	*iamdomain.IAMServiceImpl

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
	iam       *iamdomain.IAMServiceImpl
	sts       *stsdomain.STSServiceImpl
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

	masterKey, err := iamdomain.GenerateMasterKey()
	require.NoError(t, err)
	e.iam, err = iamdomain.NewIAMServiceImpl(t.Context(), e.iamNC, masterKey)
	require.NoError(t, err)
	iamJS := testutil.NewJetStream(t, e.iamNC)
	e.sts, err = stsdomain.NewSTSServiceImpl(t.Context(), e.iamNC, e.iam, masterKey, ekscluster.NewOIDCJWKSReader(iamJS))
	require.NoError(t, err)
	if !opts.noRevoker {
		e.iam.SetSessionRevoker(e.sts)
	}

	encrypted, err := iamdomain.EncryptSecret(cbRootSecret, masterKey)
	require.NoError(t, err)
	require.NoError(t, e.iam.SeedBootstrap(&iamdomain.BootstrapData{
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
	gw := gwfix.WithDeps(&GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
	}, gwfix.Deps{NATS: e.nc, Nodes: 1, IAM: e.counter, STS: e.sts})
	withIAMAndSTS(withRealAuth(gw))
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
		gwfix.SignRequestAs(t, req, body, c.akid, c.secret, service, time.Now().UTC())
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
	usersKV, err := e.js.KeyValue(t.Context(), iamdomain.KVBucketUsers)
	require.NoError(t, err)
	entry, err := usersKV.Get(t.Context(), e.accountID+"."+user)
	require.NoError(t, err)
	var u iamdomain.User
	require.NoError(t, json.Unmarshal(entry.Value(), &u))
	u.AccessKeys = []string{}
	data, err := json.Marshal(u)
	require.NoError(t, err)
	_, err = usersKV.Put(t.Context(), e.accountID+"."+user, data)
	require.NoError(t, err)
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
		require.NoError(t, e.js.DeleteKeyValue(t.Context(), iamdomain.KVBucketAccessKeys))
		for name, c := range map[string]cbCreds{"known": e.alice, "unknown": {akid: cbUnknownKey, secret: cbBadSecret}} {
			assert.Equal(t, internal, cbOutcomeOf(e.request(t, e.h, c, "sts", gci)), name)
		}
	})

	t.Run("session_bucket_deleted", func(t *testing.T) {
		e := newCBEnv(t, cbEnvOpts{})
		roleARN, _ := e.createAssumableRole(t, "app")
		s := e.assumeRole(t, roleARN, "s1")
		require.NoError(t, e.js.DeleteKeyValue(t.Context(), stsdomain.KVBucketSessionCredentials))
		assert.Equal(t, internal, cbOutcomeOf(e.callerIdentity(t, s)))
	})

	t.Run("accounts_bucket_deleted", func(t *testing.T) {
		e := newCBEnv(t, cbEnvOpts{})
		require.NoError(t, e.js.DeleteKeyValue(t.Context(), iamdomain.KVBucketAccounts))
		// Fails closed only once the signature holds; a bad one still reads as such.
		assert.Equal(t, internal, cbOutcomeOf(e.callerIdentity(t, e.alice)))
		assert.Equal(t, cbRegistry(awserrors.ErrorSignatureDoesNotMatch),
			cbOutcomeOf(e.callerIdentity(t, cbCreds{akid: e.alice.akid, secret: cbBadSecret})))
	})
}
