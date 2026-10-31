package awsgw_test

import (
	"context"
	"encoding/json"
	"github.com/go-chi/chi/v5"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	admissionquota "github.com/mulgadc/spinifex/spinifex/domains/admission/quota"
	ec2awsapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi"
	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/gateway"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/audit"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/accountadmission"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/clusterready"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/credverify"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/ec2quota"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/policyauth"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"
	"time"
)

// policyRetryBase shortens the backoff between policy-resolve attempts.
var policyRetryBase = 200 * time.Millisecond

// withVerifier authenticates through the same IAM and STS instances the config
// holds and fills any unset policy, admission and readiness capability from
// them, as the awsgw role composes them.
func withVerifier(gw *gateway.GatewayConfig) *gateway.GatewayConfig {
	gw.Credentials = credverify.New(gw.IAMService, gw.STSService)
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
