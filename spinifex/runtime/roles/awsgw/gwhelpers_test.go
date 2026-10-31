package awsgw_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/go-chi/chi/v5"
	"github.com/mulgadc/bluebottle/pkg/auth"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	admissionquota "github.com/mulgadc/spinifex/spinifex/domains/admission/quota"
	ec2awsapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi"
	ecrauth "github.com/mulgadc/spinifex/spinifex/domains/ecr/auth"
	ecrregistry "github.com/mulgadc/spinifex/spinifex/domains/ecr/registry"
	"github.com/mulgadc/spinifex/spinifex/domains/ecs/awsapi"
	eksawsapi "github.com/mulgadc/spinifex/spinifex/domains/eks/awsapi"
	ekscluster "github.com/mulgadc/spinifex/spinifex/domains/eks/cluster"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	stsawsapi "github.com/mulgadc/spinifex/spinifex/domains/sts/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/passrole"
	"github.com/mulgadc/spinifex/spinifex/gateway"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/audit"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
	ingresshttp "github.com/mulgadc/spinifex/spinifex/ingress/http"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/accountadmission"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/clusterready"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/credverify"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/ec2quota"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/policyauth"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/registryadapt"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// policyRetryBase shortens the backoff between policy-resolve attempts.
var policyRetryBase = 200 * time.Millisecond

// withCapabilities fills any unset policy, admission and readiness capability
// from the config's IAM service and bus, as the awsgw role composes them.
func withCapabilities(gw *gateway.GatewayConfig) *gateway.GatewayConfig {
	if gw.Policy == nil {
		gw.Policy = policyauth.New(gw.IAMService, policyauth.WithRetryBase(policyRetryBase))
	}
	if gw.Admission == nil {
		gw.Admission = accountadmission.New(gw.IAMService)
	}
	if gw.Readiness == nil && gw.NATSConn != nil {
		gw.Readiness = clusterready.New(gw.NATSConn)
	}
	return gw
}

// withVerifier authenticates through the same IAM and STS instances the config
// holds, so a test that changes one of them changes authentication too.
func withVerifier(gw *gateway.GatewayConfig) *gateway.GatewayConfig {
	gw.Credentials = credverify.New(gw.IAMService, gw.STSService)
	return withCapabilities(gw)
}

const (
	authzAccountID = "123456789012"
	authzRegion    = "ap-southeast-2"
)

// doRequest sends a request through an http.Handler and returns the response.
func doRequest(handler http.Handler, req *http.Request) *http.Response {
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w.Result()
}

func decodeJSONError(t *testing.T, resp *http.Response) jsonErrorEnvelope {
	t.Helper()
	var out jsonErrorEnvelope
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

func setupTestApp(accessKey, secretKey string) http.Handler {
	encryptedSecret, err := iamdomain.EncryptSecret(secretKey, gwfix.MasterKey)
	if err != nil {
		panic("failed to encrypt test secret: " + err.Error())
	}

	mockSvc := &gwfix.IAM{
		MasterKey: gwfix.MasterKey,
		AccessKeys: map[string]*iamdomain.AccessKey{
			accessKey: {
				AccessKeyID:     accessKey,
				SecretAccessKey: encryptedSecret,
				UserName:        "root",
				Status:          "Active",
			},
		},
	}

	gw := withVerifier(&gateway.GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
		IAMService:     mockSvc,
	})
	withServices(gw, awsapi.NewRegistration(awsapi.Deps{}), ec2Registration(gw))

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})

	return r
}

// allowAllIAMService resolves an allow-everything policy for every principal.
// Tests that exercise handler behaviour rather than authorization use it so the
// policy gate passes on a real evaluation instead of a bypass.
func allowAllIAMService() *gwfix.PolicyIAM {
	allow := []iamdomain.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []iamdomain.Statement{
			{Effect: "Allow", Action: iamdomain.StringOrArr{"*"}, Resource: iamdomain.StringOrArr{"*"}},
		},
	}}
	return &gwfix.PolicyIAM{
		GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) { return allow, nil },
		GetRolePoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) { return allow, nil },
	}
}

// auditField returns one value from the record's log fields, or "" when the
// record omits it as empty.
func auditField(rec *audit.Record, key string) string {
	f := rec.Fields()
	for i := 0; i+1 < len(f); i += 2 {
		if f[i] == key {
			v, _ := f[i+1].(string)
			return v
		}
	}
	return ""
}

// jsonErrorEnvelope is the AWS JSON 1.1 error body shape.
type jsonErrorEnvelope struct {
	Type    string `json:"__type"`
	Message string `json:"message"`
}

// withServices makes regs the gateway's registered services.
func withServices(gw *gateway.GatewayConfig, regs ...dispatch.Registration) *gateway.GatewayConfig {
	b := dispatch.NewBuilder()
	for _, reg := range regs {
		if err := b.Register(reg); err != nil {
			panic(err)
		}
	}
	gw.Services = b.Build()
	return gw
}

// ec2Registration composes the EC2 registration from the NATS connection, IAM
// service and node count gw holds, as the awsgw role does. mutate adjusts the
// rest.
func ec2Registration(gw *gateway.GatewayConfig, mutate ...func(*ec2awsapi.Deps)) dispatch.Registration {
	deps := ec2awsapi.Deps{
		NATS:          gw.NATSConn,
		IAM:           gw.IAMService,
		Quota:         ec2quota.Adapter{Service: quotaFor(gw)},
		ActiveNodes:   gw.DiscoverActiveNodes,
		ExpectedNodes: gw.ExpectedNodes,
	}
	for _, m := range mutate {
		m(&deps)
	}
	return ec2awsapi.NewRegistration(deps)
}

func quotaFor(gw *gateway.GatewayConfig) *admissionquota.Service {
	q, _ := quotaByGateway.Load(gw)
	svc, _ := q.(*admissionquota.Service)
	return svc
}

// quotaByGateway carries the quota service a test built for a gateway, which no
// longer holds one: the registration helpers read it back to compose Deps.
var quotaByGateway sync.Map

func allowAllTaggingDocs() []iamdomain.PolicyDocument {
	return []iamdomain.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []iamdomain.Statement{
			{Effect: "Allow", Action: iamdomain.StringOrArr{"*"}, Resource: iamdomain.StringOrArr{"*"}},
		},
	}}
}

func normalizeRequestID(s string) string {
	return requestIDPattern.ReplaceAllString(s, "<RequestId>ID</RequestId>")
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}

func scopedTaggingDocs(effect, action, resource string) []iamdomain.PolicyDocument {
	return []iamdomain.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []iamdomain.Statement{
			{Effect: effect, Action: iamdomain.StringOrArr{action}, Resource: iamdomain.StringOrArr{resource}},
		},
	}}
}

// withPrincipalField sets one field on the caller sealed in ctx, sealing a new
// caller when ctx has none. The record seals a caller once, so this rebuilds the
// context from the recorded facts when one is already sealed; it then keeps no
// other value ctx carries, which the hand-built requests it is used on do not have.
func withPrincipalField(ctx context.Context, f principalField, v string) context.Context {
	p, sealed := reqctx.Principal(ctx)
	key := reqctx.AccessKeyID(ctx)
	switch f {
	case fieldIdentity:
		p.Identity = v
	case fieldAccountID:
		p.AccountID = v
	case fieldAccessKey:
		key = v
	case fieldPrincipalType:
		p.PrincipalType = v
	case fieldAssumedRoleARN:
		p.AssumedRoleARN = v
	case fieldAssumedRoleID:
		p.AssumedRoleID = v
	case fieldUnderlyingRoleARN:
		p.UnderlyingRoleARN = v
	case fieldUserID:
		p.UserID = v
	case fieldUserARN:
		p.UserARN = v
	}
	p.CallerARN = ""
	if arn, err := credverify.CallerARN(p); err == nil {
		p.CallerARN = arn
	}
	if !sealed {
		return reqctx.WithPrincipal(ctx, p, key)
	}
	fresh := context.Background()
	if svc := reqctx.Service(ctx); svc != "" {
		fresh = reqctx.WithService(fresh, svc)
	}
	if region := reqctx.Region(ctx); region != "" {
		fresh = reqctx.WithRegion(fresh, region)
	}
	if action := reqctx.Action(ctx); action != "" {
		fresh = reqctx.WithAction(fresh, action)
	}
	return reqctx.WithPrincipal(fresh, p, key)
}

const (
	fieldIdentity principalField = iota
	fieldAccountID
	fieldAccessKey
	fieldPrincipalType
	fieldAssumedRoleARN
	fieldAssumedRoleID
	fieldUnderlyingRoleARN
	fieldUserID
	fieldUserARN
)

// withTestIdentity attaches the identity half of the auth context the SigV4
// middleware sets, so a hand-built request reaches the handler instead of being
// denied. Existing values are preserved for tests that set their own principal.
func withTestIdentity(req *http.Request) *http.Request {
	ctx := req.Context()
	have, _ := reqctx.Principal(ctx)
	if have.Identity == "" {
		ctx = withPrincipalField(ctx, fieldIdentity, "alice")
	}
	if have.PrincipalType == "" {
		ctx = withPrincipalField(ctx, fieldPrincipalType, gwfix.PrincipalTypeUser)
	}
	return req.WithContext(ctx)
}

var requestIDPattern = regexp.MustCompile(`<RequestId>[^<]*</RequestId>`)

// principalField names one field of the sealed principal, so a test can
// build a caller one fact at a time.
type principalField int

func withQuota(gw *gateway.GatewayConfig, q *admissionquota.Service) *gateway.GatewayConfig {
	quotaByGateway.Store(gw, q)
	return gw
}

// cbOutcome is the wire verdict a client sees: status, AWS error code, message.
type cbOutcome struct {
	status  int
	code    string
	message string
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

func (e *cbEnv) assumeRole(t *testing.T, roleARN, session string) cbCreds {
	t.Helper()
	return cbSessionFrom(t, e.request(t, e.h, e.alice, "sts",
		cbForm("AssumeRole", "RoleArn", roleARN, "RoleSessionName", session)))
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

// gateway builds a router over the env's services with its own (cold) caches.
func (e *cbEnv) gateway() http.Handler {
	gw := &gateway.GatewayConfig{
		DisableLogging: true,
		NATSConn:       e.nc,
		Region:         gwfix.Region,
		ExpectedNodes:  1,
		IAMService:     e.counter,
		Credentials:    credverify.New(e.counter, e.sts),
		STSService:     e.sts,
	}
	withCapabilities(gw)
	withIAMAndSTS(gw)
	return gw.SetupRoutes()
}

func (e *cbEnv) newKey(t *testing.T, user string) cbCreds {
	t.Helper()
	k, err := e.iam.CreateAccessKey(e.accountID, &iam.CreateAccessKeyInput{UserName: aws.String(user)})
	require.NoError(t, err)
	return cbCreds{akid: aws.StringValue(k.AccessKey.AccessKeyId), secret: aws.StringValue(k.AccessKey.SecretAccessKey)}
}

func (e *cbEnv) newUser(t *testing.T, name string) (cbCreds, string) {
	t.Helper()
	u, err := e.iam.CreateUser(e.accountID, &iam.CreateUserInput{UserName: aws.String(name)})
	require.NoError(t, err)
	return e.newKey(t, name), aws.StringValue(u.User.UserId)
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

type cbCreds struct {
	akid, secret, token string
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

type cbEnvOpts struct {
	// noRevoker leaves IAM without the STS session revoker, standing in for a
	// revocation that failed at delete time.
	noRevoker bool
	// separateIAMConn gives IAM and STS their own NATS connection, so it can be
	// closed while the gateway's own connection stays up.
	separateIAMConn bool
}

func cbOutcomeOf(rec *httptest.ResponseRecorder) cbOutcome {
	body := rec.Body.String()
	return cbOutcome{status: rec.Code, code: cbXML(body, "Code"), message: cbXML(body, "Message")}
}

func cbRegistry(code string) cbOutcome {
	m := awserrors.ErrorLookup[code]
	return cbOutcome{status: m.HTTPCode, code: code, message: m.Message}
}

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

const (
	cbRootAKID   = "AKIACBROOTKEYAAAAAAA"
	cbRootSecret = "cb-root-secret-0123456789abcdefghijklmn"
	cbUnknownKey = "AKIACBUNKNOWNKEYAAAA"
	cbUnknownSes = "ASIACBUNKNOWNSESSION"
	cbBadSecret  = "WRONG-secret-0123456789abcdefghijklmnop"
)

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

func cbSessionFrom(t *testing.T, rec *httptest.ResponseRecorder) cbCreds {
	t.Helper()
	require.Equalf(t, http.StatusOK, rec.Code, "mint failed: %s", rec.Body.String())
	body := rec.Body.String()
	c := cbCreds{akid: cbXML(body, "AccessKeyId"), secret: cbXML(body, "SecretAccessKey"), token: cbXML(body, "SessionToken")}
	require.True(t, strings.HasPrefix(c.akid, "ASIA"), "session AKID %q", c.akid)
	require.NotEmpty(t, c.token)
	return c
}

// withIAMAndSTS registers IAM and STS together on gw, with the unsigned
// web-identity capability, as awsgw composes them.
func withIAMAndSTS(gw *gateway.GatewayConfig) *gateway.GatewayConfig {
	gw.AnonymousSTS = stsawsapi.NewAnonymousAssumeRoleWithWebIdentity(stsawsapi.Deps{STS: gw.STSService, IAM: gw.IAMService})
	return withServices(gw, iamRegistration(gw), stsRegistration(gw))
}

const (
	testDiscAccount = "000000000001"
	testDiscCluster = "h1"
	testDiscIssuer  = "https://10.0.0.1:9999/oidc/eks/ap-southeast-2/000000000001/h1"
)

// cbIAMActions are the IAM actions these tests send; every other action is STS.
var cbIAMActions = map[string]bool{"ListUsers": true, "CreateUser": true, "GetUser": true}

// stsRegistration is the STS registration over the IAMService and STSService
// gw holds, as the awsgw role composes it. Set them on gw first.
func stsRegistration(gw *gateway.GatewayConfig) dispatch.Registration {
	return stsawsapi.NewRegistration(stsawsapi.Deps{STS: gw.STSService, IAM: gw.IAMService})
}

func statement(effect, action, resource string) iamdomain.Statement {
	return iamdomain.Statement{
		Effect:   effect,
		Action:   iamdomain.StringOrArr{action},
		Resource: iamdomain.StringOrArr{resource},
	}
}

// withContainers registers ECS and EKS on gw with the NATS connection and
// mock IAM service it holds, as the awsgw role composes them.
func withContainers(gw *gateway.GatewayConfig) *gateway.GatewayConfig {
	b := dispatch.NewBuilder()
	if err := b.Register(awsapi.NewRegistration(awsapi.Deps{NATS: gw.NATSConn})); err != nil {
		panic(err)
	}
	roles, _ := gw.IAMService.(IAMServiceLike)
	if err := b.Register(eksawsapi.NewRegistration(eksawsapi.Deps{NATS: gw.NATSConn, Roles: mockRoleResolver{iam: roles}})); err != nil {
		panic(err)
	}
	gw.Services = b.Build()
	return gw
}

// newTestRegistryHandler composes the /v2 handler the way the awsgw role does, over
// gw's IAM and STS services, with registry serving authorized requests.
func newTestRegistryHandler(t *testing.T, gw *gateway.GatewayConfig, iss *ecrauth.Issuer, verify *ecrauth.Verifier, registry ...http.Handler) http.Handler {
	t.Helper()
	cfg := ecrregistry.BridgeConfig{
		Verifier:  verify,
		Issuer:    iss,
		Admission: accountadmission.New(gw.IAMService),
		Region:    gw.Region,
	}
	cfg = registryadapt.Config(cfg, credverify.New(gw.IAMService, gw.STSService),
		policyauth.New(gw.IAMService, policyauth.WithRetryBase(time.Millisecond)))
	if len(registry) > 0 {
		cfg.Registry = registry[0]
	}
	b, err := ecrregistry.NewBridge(cfg)
	require.NoError(t, err)
	return ingresshttp.NewHostMatcher(gw.InternalSuffix, "dkr", "ecr").Middleware(b.Handler())
}

// IAMServiceLike is the slice of the IAM service the mock resolver reads.
type IAMServiceLike interface {
	CanonicalResourceARN(accountID string, kind arn.IAMResourceType, name string) (string, error)
}

// mockRoleResolver resolves roles through the gateway's mock IAM service, the
// way the IAM-owned resolver does against the real store.
type mockRoleResolver struct{ iam IAMServiceLike }

func (m mockRoleResolver) ResolveRole(accountID, roleARN string) (passrole.Role, error) {
	roleAccount, name, err := auth.ParseRoleARN(roleARN)
	if err != nil {
		return passrole.Role{}, passrole.ErrMalformed
	}
	if roleAccount != accountID {
		return passrole.Role{}, passrole.ErrForeignAccount
	}
	canonical, err := m.iam.CanonicalResourceARN(roleAccount, arn.IAMRole, name)
	if err != nil {
		if awserrors.IsErrorCode(err, awserrors.ErrorIAMNoSuchEntity) {
			return passrole.Role{}, passrole.ErrNotFound
		}
		return passrole.Role{}, err
	}
	if canonical != roleARN {
		return passrole.Role{}, passrole.ErrNotFound
	}
	return passrole.Role{ARN: canonical, Name: name}, nil
}
