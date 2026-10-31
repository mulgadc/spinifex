package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/go-chi/chi/v5"
	telemetryv1 "github.com/mulgadc/spinifex/contracts/telemetry/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	acmawsapi "github.com/mulgadc/spinifex/spinifex/domains/acm/awsapi"
	admissionquota "github.com/mulgadc/spinifex/spinifex/domains/admission/quota"
	ec2instanceapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/instance"
	ec2tags "github.com/mulgadc/spinifex/spinifex/domains/ec2/tags"
	ecrauth "github.com/mulgadc/spinifex/spinifex/domains/ecr/auth"
	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
	elbv2awsapi "github.com/mulgadc/spinifex/spinifex/domains/elbv2/awsapi"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	iamawsapi "github.com/mulgadc/spinifex/spinifex/domains/iam/awsapi"
	rdsawsapi "github.com/mulgadc/spinifex/spinifex/domains/rds/awsapi"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	stsawsapi "github.com/mulgadc/spinifex/spinifex/domains/sts/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/tagging"
	taggingawsapi "github.com/mulgadc/spinifex/spinifex/domains/tagging/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
	operatoradmin "github.com/mulgadc/spinifex/spinifex/operator/admin"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/credverify"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// withACM registers the ACM control plane on gw backed by deps, as the awsgw
// role composes it. An unset deps.NATS takes gw.NATSConn.
func withACM(gw *GatewayConfig, deps acmawsapi.Deps) *GatewayConfig {
	if deps.NATS == nil {
		deps.NATS = gw.NATSConn
	}
	b := dispatch.NewBuilder()
	if err := b.Register(acmawsapi.NewRegistration(deps)); err != nil {
		panic(err)
	}
	gw.Services = b.Build()
	return gw
}

// acmPipelineApp wires SigV4AuthMiddleware -> Request as SetupRoutes does, on
// live test NATS, with docs as every principal's policy. The region is
// gwfix.Region because gwfix.SignRequestAs always signs for it.
func acmPipelineApp(t *testing.T, docs []iamdomain.PolicyDocument) (*GatewayConfig, http.Handler) {
	t.Helper()
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	require.NoError(t, err)
	_, nc := testutil.StartTestNATS(t)

	gw := withVerifier(&GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
		NATSConn:       nc,
		IAMService: &gwfix.PolicyIAM{
			IAM: gwfix.IAM{
				MasterKey: gwfix.MasterKey,
				AccessKeys: map[string]*iamdomain.AccessKey{
					gwfix.AccessKey: {
						AccessKeyID:     gwfix.AccessKey,
						SecretAccessKey: encryptedSecret,
						UserName:        "alice",
						AccountID:       authzAccountID,
						Status:          "Active",
					},
				},
			},
			GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) { return docs, nil },
		},
	})

	withACM(gw, acmawsapi.Deps{})

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", gw.Request)
	return gw, r
}

// allowAllACMDocs is the policy acmPipelineApp uses when the test is not
// exercising authorization itself.
func allowAllACMDocs() []iamdomain.PolicyDocument {
	return []iamdomain.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []iamdomain.Statement{
			{Effect: "Allow", Action: iamdomain.StringOrArr{"*"}, Resource: iamdomain.StringOrArr{"*"}},
		},
	}}
}

// subscribeACMCounter answers subject with payload and counts invocations, so
// a denial can be proven to short-circuit before the handler ever runs.
func subscribeACMCounter(t *testing.T, nc *nats.Conn, subject string, payload []byte) *int {
	t.Helper()
	calls := 0
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		calls++
		_ = msg.Respond(payload)
	})
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return &calls
}

// signACMRequest builds a POST / signed with credential-scope service "acm",
// the shape every ACM JSON 1.1 client sends.
func signACMRequest(t *testing.T, target string, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Host = "localhost:9999"
	if target != "" {
		req.Header.Set("X-Amz-Target", target)
	}
	gwfix.SignRequestAs(t, req, body, gwfix.AccessKey, gwfix.SecretKey, "acm", time.Now().UTC())
	return req
}

func setupRDSRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	ctx := reqctx.WithService(req.Context(), "rds")
	ctx = withPrincipalField(ctx, fieldAccountID, rdsTestAccountID)
	return withTestIdentity(req.WithContext(ctx))
}

// Counts policy resolutions, so a test can tell an authorized request from one
// that failed some other guard before the evaluator was ever reached.
type rdsPolicyProbe struct{ consulted int }

// A gateway whose IAM service answers with policies, so the RDS dispatch path
// runs a real evaluation for either principal branch.
func newRDSPolicyGateway(policies []iamdomain.PolicyDocument) (*GatewayConfig, *rdsPolicyProbe) {
	return newRDSGatewayWithResolver(func(_, _ string) ([]iamdomain.PolicyDocument, error) {
		return policies, nil
	})
}

// A signed customer request: an IAM user with a resolvable identity, which is
// what turns the policy check from the pre-IAM bypass into a real evaluation.
func setupRDSUserRequest(body, accountID string) *http.Request {
	req := setupRDSRequest(body)
	ctx := withPrincipalField(req.Context(), fieldAccountID, accountID)
	ctx = withPrincipalField(ctx, fieldIdentity, "alice")
	ctx = withPrincipalField(ctx, fieldPrincipalType, gwfix.PrincipalTypeUser)
	return req.WithContext(ctx)
}

const rdsTestAccountID = "123456789012"

// rdsRegistrationInventory is the inventory the RDS registration declares to a
// registry, keyed as AWSOperationInventory expects.
func rdsRegistrationInventory() map[string]dispatch.Inventory {
	return withRDS(&GatewayConfig{}).Services.Inventory()
}

// serveRDS dispatches r to the registered RDS service exactly as Request does,
// registering it first from the fields gw holds when the test has not.
func (gw *GatewayConfig) serveRDS(w http.ResponseWriter, r *http.Request) error {
	if gw.Services == nil {
		withRDS(gw)
	}
	entry, ok := gw.registered(rdsawsapi.ServiceName)
	if !ok {
		panic("RDS is not registered on this gateway")
	}
	return gw.dispatchRegistered(entry, w, r)
}

func newRDSGatewayWithResolver(
	resolve func(accountID, name string) ([]iamdomain.PolicyDocument, error),
) (*GatewayConfig, *rdsPolicyProbe) {
	probe := &rdsPolicyProbe{}
	counted := func(accountID, name string) ([]iamdomain.PolicyDocument, error) {
		probe.consulted++
		return resolve(accountID, name)
	}
	// Both branches are wired: an assumed-role principal resolves by role, and a
	// test that only stubbed users would miss it entirely.
	return withVerifier(&GatewayConfig{
		DisableLogging: true,
		Region:         "ap-southeast-2",
		IAMService: &gwfix.PolicyIAM{
			GetUserPoliciesFn: counted,
			GetRolePoliciesFn: counted,
		},
	}), probe
}

// withRDS registers RDS on gw with the NATS connection and node count it holds,
// as the awsgw role composes it. Set both on gw before calling.
func withRDS(gw *GatewayConfig) *GatewayConfig {
	deps := rdsawsapi.Deps{NATS: gw.NATSConn, ExpectedNodes: gw.ExpectedNodes}
	if quotaFor(gw) != nil {
		deps.Quota = quotaFor(gw)
	}
	b := dispatch.NewBuilder()
	if err := b.Register(rdsawsapi.NewRegistration(deps)); err != nil {
		panic(err)
	}
	gw.Services = b.Build()
	return gw
}

// withELBv2 registers ELBv2 on gw with the NATS connection and quota it holds,
// as the awsgw role composes it. Set both on gw before calling.
func withELBv2(gw *GatewayConfig) *GatewayConfig {
	b := dispatch.NewBuilder()
	reg := elbv2awsapi.NewRegistration(elbv2awsapi.Deps{NATS: gw.NATSConn, Quota: quotaFor(gw)})
	if err := b.Register(reg); err != nil {
		panic(err)
	}
	gw.Services = b.Build()
	return gw
}

// serveAccountLimits runs DescribeAccountLimits through the service with live
// NATS, so the request is dispatched rather than stopped at the NATS guard.
func serveAccountLimits(t *testing.T, gw *GatewayConfig, accountID, body string) ([]byte, error) {
	t.Helper()
	_, nc := testutil.StartTestNATS(t)
	gw.NATSConn = nc
	gw.IAMService = allowAllIAMService()
	withELBv2(gw)

	w := httptest.NewRecorder()
	if err := gw.serveELBv2(w, elbv2Request(accountID, body)); err != nil {
		return nil, err
	}
	return w.Body.Bytes(), nil
}

// accountLimitsResponse is the wire shape a caller parses, so the test asserts
// on what leaves the gateway rather than on the struct behind it.
type accountLimitsResponse struct {
	XMLName xml.Name `xml:"DescribeAccountLimitsResponse"`
	Limits  []struct {
		Name string `xml:"Name"`
		Max  string `xml:"Max"`
	} `xml:"DescribeAccountLimitsResult>Limits>member"`
}

// describeAccountLimits decodes the XML DescribeAccountLimits writes, keyed by
// limit name.
func describeAccountLimits(t *testing.T, gw *GatewayConfig, accountID string) map[string]string {
	t.Helper()

	body, err := serveAccountLimits(t, gw, accountID, "Action=DescribeAccountLimits")
	require.NoError(t, err)

	var decoded accountLimitsResponse
	require.NoError(t, xml.Unmarshal(body, &decoded))
	require.NotEmpty(t, decoded.Limits)

	byName := make(map[string]string, len(decoded.Limits))
	for _, limit := range decoded.Limits {
		byName[limit.Name] = limit.Max
	}
	return byName
}

// elbv2PipelineApp wires SigV4AuthMiddleware -> Request as SetupRoutes does, on
// live test NATS, with docs as every principal's policy.
func elbv2PipelineApp(t *testing.T, docs []iamdomain.PolicyDocument, quota *admissionquota.Service) (*GatewayConfig, http.Handler) {
	t.Helper()
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	require.NoError(t, err)
	_, nc := testutil.StartTestNATS(t)

	gw := withVerifier(&GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
		NATSConn:       nc,
		IAMService: &gwfix.PolicyIAM{
			IAM: gwfix.IAM{
				MasterKey: gwfix.MasterKey,
				AccessKeys: map[string]*iamdomain.AccessKey{
					gwfix.AccessKey: {
						AccessKeyID:     gwfix.AccessKey,
						SecretAccessKey: encryptedSecret,
						UserName:        "alice",
						AccountID:       authzAccountID,
						Status:          "Active",
					},
				},
			},
			GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) { return docs, nil },
		},
	})
	withELBv2(withQuota(gw, quota))

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", gw.Request)
	return gw, r
}

func pipelineListener(lbName, lbID, id string) string {
	return arn.FormatELBv2Listener(gwfix.Region, authzAccountID, lbName, lbID, id, "application")
}

// signELBv2Request builds a signed Query-protocol POST for the
// elasticloadbalancing credential scope.
func signELBv2Request(t *testing.T, body string) *http.Request {
	t.Helper()
	b := []byte(body)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b))
	req.Host = "localhost:9999"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	gwfix.SignRequestAs(t, req, b, gwfix.AccessKey, gwfix.SecretKey, "elasticloadbalancing", time.Now().UTC())
	return req
}

// serveELBv2 dispatches r to the registered ELBv2 service exactly as Request
// does past its NATS gate, registering it first from the fields gw holds when
// the test has not.
func (gw *GatewayConfig) serveELBv2(w http.ResponseWriter, r *http.Request) error {
	if gw.Services == nil {
		withELBv2(gw)
	}
	entry, ok := gw.registered(elbv2awsapi.ServiceName)
	if !ok {
		panic("ELBv2 is not registered on this gateway")
	}
	return gw.dispatchRegistered(entry, w, r)
}

// elbv2Request builds an authenticated Query request for body, as the SigV4
// middleware would hand it to the service.
func elbv2Request(accountID, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	ctx := reqctx.WithService(req.Context(), "elasticloadbalancing")
	ctx = withPrincipalField(ctx, fieldAccountID, accountID)
	return withTestIdentity(req.WithContext(ctx))
}

// withServices makes regs the gateway's registered services.
func withServices(gw *GatewayConfig, regs ...dispatch.Registration) *GatewayConfig {
	b := dispatch.NewBuilder()
	for _, reg := range regs {
		if err := b.Register(reg); err != nil {
			panic(err)
		}
	}
	gw.Services = b.Build()
	return gw
}

// withIAM registers IAM alone on gw.
func withIAM(gw *GatewayConfig) *GatewayConfig {
	return withServices(gw, iamRegistration(gw))
}

// iamRegistration is the IAM registration over the IAMService, NATS connection
// and node count gw holds, as the awsgw role composes it. Set them on gw first.
func iamRegistration(gw *GatewayConfig) dispatch.Registration {
	return iamawsapi.NewRegistration(iamawsapi.Deps{
		IAM: gw.IAMService,
		CountInstanceProfileAssociations: func(ctx context.Context, accountID, profileARN string) (int, error) {
			return ec2instanceapi.CountInstanceProfileAssociations(ctx, gw.NATSConn, gw.DiscoverActiveNodes(ctx), accountID, profileARN)
		},
	})
}

// flexMockIAMService is a configurable mock with per-method overrides. It
// embeds the interface so it satisfies the full contract; only the fn-field
// methods below are wired, and any other method nil-panics if a test reaches
// an unmocked path.
type flexMockIAMService struct {
	iamdomain.IAMService

	createUserFn           func(string, *iam.CreateUserInput) (*iam.CreateUserOutput, error)
	getUserFn              func(string, *iam.GetUserInput) (*iam.GetUserOutput, error)
	listUsersFn            func(string, *iam.ListUsersInput) (*iam.ListUsersOutput, error)
	deleteUserFn           func(string, *iam.DeleteUserInput) (*iam.DeleteUserOutput, error)
	createAccessKeyFn      func(string, *iam.CreateAccessKeyInput) (*iam.CreateAccessKeyOutput, error)
	listAccessKeysFn       func(string, *iam.ListAccessKeysInput) (*iam.ListAccessKeysOutput, error)
	deleteAccessKeyFn      func(string, *iam.DeleteAccessKeyInput) (*iam.DeleteAccessKeyOutput, error)
	updateAccessKeyFn      func(string, *iam.UpdateAccessKeyInput) (*iam.UpdateAccessKeyOutput, error)
	canonicalResourceARNFn func(string, arn.IAMResourceType, string) (string, error)
	lookupAccessKeyFn      func(string) (*iamdomain.AccessKey, error)

	getAccountSummaryFn func(string, *iam.GetAccountSummaryInput) (*iam.GetAccountSummaryOutput, error)
	getRoleFn           func(string, *iam.GetRoleInput) (*iam.GetRoleOutput, error)
}

func (m *flexMockIAMService) AddUserToGroup(_ string, _ *iam.AddUserToGroupInput) (*iam.AddUserToGroupOutput, error) {
	return &iam.AddUserToGroupOutput{}, nil
}

func (m *flexMockIAMService) AttachGroupPolicy(_ string, _ *iam.AttachGroupPolicyInput) (*iam.AttachGroupPolicyOutput, error) {
	return &iam.AttachGroupPolicyOutput{}, nil
}

func (m *flexMockIAMService) CanonicalResourceARN(accountID string, kind arn.IAMResourceType, name string) (string, error) {
	if m.canonicalResourceARNFn != nil {
		return m.canonicalResourceARNFn(accountID, kind, name)
	}
	return arn.FormatIAMPath(kind, accountID, "/", name), nil
}

func (m *flexMockIAMService) CreateAccessKey(accountID string, input *iam.CreateAccessKeyInput) (*iam.CreateAccessKeyOutput, error) {
	if m.createAccessKeyFn != nil {
		return m.createAccessKeyFn(accountID, input)
	}
	return &iam.CreateAccessKeyOutput{}, nil
}

func (m *flexMockIAMService) CreateGroup(_ string, _ *iam.CreateGroupInput) (*iam.CreateGroupOutput, error) {
	return &iam.CreateGroupOutput{}, nil
}

func (m *flexMockIAMService) CreateUser(accountID string, input *iam.CreateUserInput) (*iam.CreateUserOutput, error) {
	if m.createUserFn != nil {
		return m.createUserFn(accountID, input)
	}
	return &iam.CreateUserOutput{}, nil
}

func (m *flexMockIAMService) DeleteAccessKey(accountID string, input *iam.DeleteAccessKeyInput) (*iam.DeleteAccessKeyOutput, error) {
	if m.deleteAccessKeyFn != nil {
		return m.deleteAccessKeyFn(accountID, input)
	}
	return &iam.DeleteAccessKeyOutput{}, nil
}

func (m *flexMockIAMService) DeleteGroup(_ string, _ *iam.DeleteGroupInput) (*iam.DeleteGroupOutput, error) {
	return &iam.DeleteGroupOutput{}, nil
}

func (m *flexMockIAMService) DeleteGroupPolicy(_ string, _ *iam.DeleteGroupPolicyInput) (*iam.DeleteGroupPolicyOutput, error) {
	return &iam.DeleteGroupPolicyOutput{}, nil
}

func (m *flexMockIAMService) DeleteUser(accountID string, input *iam.DeleteUserInput) (*iam.DeleteUserOutput, error) {
	if m.deleteUserFn != nil {
		return m.deleteUserFn(accountID, input)
	}
	return &iam.DeleteUserOutput{}, nil
}

func (m *flexMockIAMService) DeleteUserPolicy(_ string, _ *iam.DeleteUserPolicyInput) (*iam.DeleteUserPolicyOutput, error) {
	return &iam.DeleteUserPolicyOutput{}, nil
}

func (m *flexMockIAMService) DetachGroupPolicy(_ string, _ *iam.DetachGroupPolicyInput) (*iam.DetachGroupPolicyOutput, error) {
	return &iam.DetachGroupPolicyOutput{}, nil
}

func (m *flexMockIAMService) GetAccountSummary(accountID string, input *iam.GetAccountSummaryInput) (*iam.GetAccountSummaryOutput, error) {
	if m.getAccountSummaryFn != nil {
		return m.getAccountSummaryFn(accountID, input)
	}
	return &iam.GetAccountSummaryOutput{}, nil
}

func (m *flexMockIAMService) GetGroup(_ string, _ *iam.GetGroupInput) (*iam.GetGroupOutput, error) {
	return &iam.GetGroupOutput{}, nil
}

func (m *flexMockIAMService) GetGroupPolicy(_ string, _ *iam.GetGroupPolicyInput) (*iam.GetGroupPolicyOutput, error) {
	return &iam.GetGroupPolicyOutput{}, nil
}

// GetRole answers NoSuchEntity by default: the STS identity gate resolves the
// target role, and a mock that invented one would let a test pass against a
// role the account does not hold.
func (m *flexMockIAMService) GetRole(accountID string, input *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	if m.getRoleFn != nil {
		return m.getRoleFn(accountID, input)
	}
	return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
}

func (m *flexMockIAMService) GetUser(accountID string, input *iam.GetUserInput) (*iam.GetUserOutput, error) {
	if m.getUserFn != nil {
		return m.getUserFn(accountID, input)
	}
	return &iam.GetUserOutput{}, nil
}

func (m *flexMockIAMService) GetUserPolicy(_ string, _ *iam.GetUserPolicyInput) (*iam.GetUserPolicyOutput, error) {
	return &iam.GetUserPolicyOutput{}, nil
}

func (m *flexMockIAMService) ListAccessKeys(accountID string, input *iam.ListAccessKeysInput) (*iam.ListAccessKeysOutput, error) {
	if m.listAccessKeysFn != nil {
		return m.listAccessKeysFn(accountID, input)
	}
	return &iam.ListAccessKeysOutput{}, nil
}

func (m *flexMockIAMService) ListAccountAliases(_ string, _ *iam.ListAccountAliasesInput) (*iam.ListAccountAliasesOutput, error) {
	return &iam.ListAccountAliasesOutput{}, nil
}

func (m *flexMockIAMService) ListAttachedGroupPolicies(_ string, _ *iam.ListAttachedGroupPoliciesInput) (*iam.ListAttachedGroupPoliciesOutput, error) {
	return &iam.ListAttachedGroupPoliciesOutput{}, nil
}

func (m *flexMockIAMService) ListGroupPolicies(_ string, _ *iam.ListGroupPoliciesInput) (*iam.ListGroupPoliciesOutput, error) {
	return &iam.ListGroupPoliciesOutput{}, nil
}

func (m *flexMockIAMService) ListGroups(_ string, _ *iam.ListGroupsInput) (*iam.ListGroupsOutput, error) {
	return &iam.ListGroupsOutput{}, nil
}

func (m *flexMockIAMService) ListGroupsForUser(_ string, _ *iam.ListGroupsForUserInput) (*iam.ListGroupsForUserOutput, error) {
	return &iam.ListGroupsForUserOutput{}, nil
}

func (m *flexMockIAMService) ListUserPolicies(_ string, _ *iam.ListUserPoliciesInput) (*iam.ListUserPoliciesOutput, error) {
	return &iam.ListUserPoliciesOutput{}, nil
}

func (m *flexMockIAMService) ListUsers(accountID string, input *iam.ListUsersInput) (*iam.ListUsersOutput, error) {
	if m.listUsersFn != nil {
		return m.listUsersFn(accountID, input)
	}
	return &iam.ListUsersOutput{}, nil
}

func (m *flexMockIAMService) LookupAccessKey(accessKeyID string) (*iamdomain.AccessKey, error) {
	if m.lookupAccessKeyFn != nil {
		return m.lookupAccessKeyFn(accessKeyID)
	}
	return &iamdomain.AccessKey{AccessKeyID: accessKeyID, AccountID: "000000000000", UserName: "user"}, nil
}

func (m *flexMockIAMService) PutGroupPolicy(_ string, _ *iam.PutGroupPolicyInput) (*iam.PutGroupPolicyOutput, error) {
	return &iam.PutGroupPolicyOutput{}, nil
}

func (m *flexMockIAMService) PutUserPolicy(_ string, _ *iam.PutUserPolicyInput) (*iam.PutUserPolicyOutput, error) {
	return &iam.PutUserPolicyOutput{}, nil
}

func (m *flexMockIAMService) RemoveUserFromGroup(_ string, _ *iam.RemoveUserFromGroupInput) (*iam.RemoveUserFromGroupOutput, error) {
	return &iam.RemoveUserFromGroupOutput{}, nil
}

func (m *flexMockIAMService) UpdateAccessKey(accountID string, input *iam.UpdateAccessKeyInput) (*iam.UpdateAccessKeyOutput, error) {
	if m.updateAccessKeyFn != nil {
		return m.updateAccessKeyFn(accountID, input)
	}
	return &iam.UpdateAccessKeyOutput{}, nil
}

// IAM_Request dispatches r to the registered IAM service exactly as Request
// does, registering it first from the fields gw holds when the test has not.
func (gw *GatewayConfig) IAM_Request(w http.ResponseWriter, r *http.Request) error {
	if gw.Services == nil {
		withIAM(gw)
	}
	entry, ok := gw.registered(iamawsapi.ServiceName)
	if !ok {
		panic("IAM is not registered on this gateway")
	}
	return gw.dispatchRegistered(entry, w, r)
}

func loadIAMExamples(t *testing.T) iamExamplesFile {
	t.Helper()

	data, err := os.ReadFile("testdata/aws/examples-1.json")
	require.NoError(t, err)

	var examples iamExamplesFile
	require.NoError(t, json.Unmarshal(data, &examples))
	require.NotEmpty(t, examples.Examples)
	return examples
}

type echoIAMService struct {
	flexMockIAMService

	wantOutput   any
	gotAccountID string
	gotInput     any
}

func (svc *echoIAMService) AddRoleToInstanceProfile(accountID string, input *iam.AddRoleToInstanceProfileInput) (*iam.AddRoleToInstanceProfileOutput, error) {
	return echoIAMCall[iam.AddRoleToInstanceProfileInput, iam.AddRoleToInstanceProfileOutput](svc, accountID, input)
}

func (svc *echoIAMService) AddUserToGroup(accountID string, input *iam.AddUserToGroupInput) (*iam.AddUserToGroupOutput, error) {
	return echoIAMCall[iam.AddUserToGroupInput, iam.AddUserToGroupOutput](svc, accountID, input)
}

func (svc *echoIAMService) AttachGroupPolicy(accountID string, input *iam.AttachGroupPolicyInput) (*iam.AttachGroupPolicyOutput, error) {
	return echoIAMCall[iam.AttachGroupPolicyInput, iam.AttachGroupPolicyOutput](svc, accountID, input)
}

func (svc *echoIAMService) AttachRolePolicy(accountID string, input *iam.AttachRolePolicyInput) (*iam.AttachRolePolicyOutput, error) {
	return echoIAMCall[iam.AttachRolePolicyInput, iam.AttachRolePolicyOutput](svc, accountID, input)
}

func (svc *echoIAMService) AttachUserPolicy(accountID string, input *iam.AttachUserPolicyInput) (*iam.AttachUserPolicyOutput, error) {
	return echoIAMCall[iam.AttachUserPolicyInput, iam.AttachUserPolicyOutput](svc, accountID, input)
}

func (svc *echoIAMService) CreateAccessKey(accountID string, input *iam.CreateAccessKeyInput) (*iam.CreateAccessKeyOutput, error) {
	return echoIAMCall[iam.CreateAccessKeyInput, iam.CreateAccessKeyOutput](svc, accountID, input)
}

func (svc *echoIAMService) CreateGroup(accountID string, input *iam.CreateGroupInput) (*iam.CreateGroupOutput, error) {
	return echoIAMCall[iam.CreateGroupInput, iam.CreateGroupOutput](svc, accountID, input)
}

func (svc *echoIAMService) CreateInstanceProfile(accountID string, input *iam.CreateInstanceProfileInput) (*iam.CreateInstanceProfileOutput, error) {
	return echoIAMCall[iam.CreateInstanceProfileInput, iam.CreateInstanceProfileOutput](svc, accountID, input)
}

func (svc *echoIAMService) CreateOpenIDConnectProvider(accountID string, input *iam.CreateOpenIDConnectProviderInput) (*iam.CreateOpenIDConnectProviderOutput, error) {
	return echoIAMCall[iam.CreateOpenIDConnectProviderInput, iam.CreateOpenIDConnectProviderOutput](svc, accountID, input)
}

func (svc *echoIAMService) CreateRole(accountID string, input *iam.CreateRoleInput) (*iam.CreateRoleOutput, error) {
	return echoIAMCall[iam.CreateRoleInput, iam.CreateRoleOutput](svc, accountID, input)
}

func (svc *echoIAMService) CreateUser(accountID string, input *iam.CreateUserInput) (*iam.CreateUserOutput, error) {
	return echoIAMCall[iam.CreateUserInput, iam.CreateUserOutput](svc, accountID, input)
}

func (svc *echoIAMService) DeleteAccessKey(accountID string, input *iam.DeleteAccessKeyInput) (*iam.DeleteAccessKeyOutput, error) {
	return echoIAMCall[iam.DeleteAccessKeyInput, iam.DeleteAccessKeyOutput](svc, accountID, input)
}

func (svc *echoIAMService) DeleteGroupPolicy(accountID string, input *iam.DeleteGroupPolicyInput) (*iam.DeleteGroupPolicyOutput, error) {
	return echoIAMCall[iam.DeleteGroupPolicyInput, iam.DeleteGroupPolicyOutput](svc, accountID, input)
}

func (svc *echoIAMService) DeleteInstanceProfile(accountID string, input *iam.DeleteInstanceProfileInput) (*iam.DeleteInstanceProfileOutput, error) {
	return echoIAMCall[iam.DeleteInstanceProfileInput, iam.DeleteInstanceProfileOutput](svc, accountID, input)
}

func (svc *echoIAMService) DeleteRole(accountID string, input *iam.DeleteRoleInput) (*iam.DeleteRoleOutput, error) {
	return echoIAMCall[iam.DeleteRoleInput, iam.DeleteRoleOutput](svc, accountID, input)
}

func (svc *echoIAMService) DeleteRolePolicy(accountID string, input *iam.DeleteRolePolicyInput) (*iam.DeleteRolePolicyOutput, error) {
	return echoIAMCall[iam.DeleteRolePolicyInput, iam.DeleteRolePolicyOutput](svc, accountID, input)
}

func (svc *echoIAMService) DeleteUser(accountID string, input *iam.DeleteUserInput) (*iam.DeleteUserOutput, error) {
	return echoIAMCall[iam.DeleteUserInput, iam.DeleteUserOutput](svc, accountID, input)
}

func (svc *echoIAMService) DeleteUserPolicy(accountID string, input *iam.DeleteUserPolicyInput) (*iam.DeleteUserPolicyOutput, error) {
	return echoIAMCall[iam.DeleteUserPolicyInput, iam.DeleteUserPolicyOutput](svc, accountID, input)
}

func (svc *echoIAMService) GetAccountSummary(accountID string, input *iam.GetAccountSummaryInput) (*iam.GetAccountSummaryOutput, error) {
	return echoIAMCall[iam.GetAccountSummaryInput, iam.GetAccountSummaryOutput](svc, accountID, input)
}

func (svc *echoIAMService) GetInstanceProfile(accountID string, input *iam.GetInstanceProfileInput) (*iam.GetInstanceProfileOutput, error) {
	if _, ok := svc.wantOutput.(*iam.GetInstanceProfileOutput); !ok {
		return &iam.GetInstanceProfileOutput{InstanceProfile: &iam.InstanceProfile{}}, nil
	}
	return echoIAMCall[iam.GetInstanceProfileInput, iam.GetInstanceProfileOutput](svc, accountID, input)
}

func (svc *echoIAMService) GetRole(accountID string, input *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	return echoIAMCall[iam.GetRoleInput, iam.GetRoleOutput](svc, accountID, input)
}

func (svc *echoIAMService) GetUser(accountID string, input *iam.GetUserInput) (*iam.GetUserOutput, error) {
	return echoIAMCall[iam.GetUserInput, iam.GetUserOutput](svc, accountID, input)
}

func (svc *echoIAMService) ListAccessKeys(accountID string, input *iam.ListAccessKeysInput) (*iam.ListAccessKeysOutput, error) {
	return echoIAMCall[iam.ListAccessKeysInput, iam.ListAccessKeysOutput](svc, accountID, input)
}

func (svc *echoIAMService) ListAccountAliases(accountID string, input *iam.ListAccountAliasesInput) (*iam.ListAccountAliasesOutput, error) {
	return echoIAMCall[iam.ListAccountAliasesInput, iam.ListAccountAliasesOutput](svc, accountID, input)
}

func (svc *echoIAMService) ListGroupPolicies(accountID string, input *iam.ListGroupPoliciesInput) (*iam.ListGroupPoliciesOutput, error) {
	return echoIAMCall[iam.ListGroupPoliciesInput, iam.ListGroupPoliciesOutput](svc, accountID, input)
}

func (svc *echoIAMService) ListGroups(accountID string, input *iam.ListGroupsInput) (*iam.ListGroupsOutput, error) {
	return echoIAMCall[iam.ListGroupsInput, iam.ListGroupsOutput](svc, accountID, input)
}

func (svc *echoIAMService) ListGroupsForUser(accountID string, input *iam.ListGroupsForUserInput) (*iam.ListGroupsForUserOutput, error) {
	return echoIAMCall[iam.ListGroupsForUserInput, iam.ListGroupsForUserOutput](svc, accountID, input)
}

func (svc *echoIAMService) ListRoleTags(accountID string, input *iam.ListRoleTagsInput) (*iam.ListRoleTagsOutput, error) {
	return echoIAMCall[iam.ListRoleTagsInput, iam.ListRoleTagsOutput](svc, accountID, input)
}

func (svc *echoIAMService) ListUserTags(accountID string, input *iam.ListUserTagsInput) (*iam.ListUserTagsOutput, error) {
	return echoIAMCall[iam.ListUserTagsInput, iam.ListUserTagsOutput](svc, accountID, input)
}

func (svc *echoIAMService) ListUsers(accountID string, input *iam.ListUsersInput) (*iam.ListUsersOutput, error) {
	return echoIAMCall[iam.ListUsersInput, iam.ListUsersOutput](svc, accountID, input)
}

func (svc *echoIAMService) PutGroupPolicy(accountID string, input *iam.PutGroupPolicyInput) (*iam.PutGroupPolicyOutput, error) {
	return echoIAMCall[iam.PutGroupPolicyInput, iam.PutGroupPolicyOutput](svc, accountID, input)
}

func (svc *echoIAMService) PutRolePolicy(accountID string, input *iam.PutRolePolicyInput) (*iam.PutRolePolicyOutput, error) {
	return echoIAMCall[iam.PutRolePolicyInput, iam.PutRolePolicyOutput](svc, accountID, input)
}

func (svc *echoIAMService) PutUserPolicy(accountID string, input *iam.PutUserPolicyInput) (*iam.PutUserPolicyOutput, error) {
	return echoIAMCall[iam.PutUserPolicyInput, iam.PutUserPolicyOutput](svc, accountID, input)
}

func (svc *echoIAMService) RemoveRoleFromInstanceProfile(accountID string, input *iam.RemoveRoleFromInstanceProfileInput) (*iam.RemoveRoleFromInstanceProfileOutput, error) {
	return echoIAMCall[iam.RemoveRoleFromInstanceProfileInput, iam.RemoveRoleFromInstanceProfileOutput](svc, accountID, input)
}

func (svc *echoIAMService) RemoveUserFromGroup(accountID string, input *iam.RemoveUserFromGroupInput) (*iam.RemoveUserFromGroupOutput, error) {
	return echoIAMCall[iam.RemoveUserFromGroupInput, iam.RemoveUserFromGroupOutput](svc, accountID, input)
}

func (svc *echoIAMService) TagRole(accountID string, input *iam.TagRoleInput) (*iam.TagRoleOutput, error) {
	return echoIAMCall[iam.TagRoleInput, iam.TagRoleOutput](svc, accountID, input)
}

func (svc *echoIAMService) TagUser(accountID string, input *iam.TagUserInput) (*iam.TagUserOutput, error) {
	return echoIAMCall[iam.TagUserInput, iam.TagUserOutput](svc, accountID, input)
}

func (svc *echoIAMService) UntagRole(accountID string, input *iam.UntagRoleInput) (*iam.UntagRoleOutput, error) {
	return echoIAMCall[iam.UntagRoleInput, iam.UntagRoleOutput](svc, accountID, input)
}

func (svc *echoIAMService) UntagUser(accountID string, input *iam.UntagUserInput) (*iam.UntagUserOutput, error) {
	return echoIAMCall[iam.UntagUserInput, iam.UntagUserOutput](svc, accountID, input)
}

func (svc *echoIAMService) UpdateAccessKey(accountID string, input *iam.UpdateAccessKeyInput) (*iam.UpdateAccessKeyOutput, error) {
	return echoIAMCall[iam.UpdateAccessKeyInput, iam.UpdateAccessKeyOutput](svc, accountID, input)
}

func (svc *echoIAMService) UpdateAssumeRolePolicy(accountID string, input *iam.UpdateAssumeRolePolicyInput) (*iam.UpdateAssumeRolePolicyOutput, error) {
	return echoIAMCall[iam.UpdateAssumeRolePolicyInput, iam.UpdateAssumeRolePolicyOutput](svc, accountID, input)
}

// setupIAMRequestHandler wires an http.Handler for IAM_Request with injected SigV4 context values.
func setupIAMRequestHandler(svc iamdomain.IAMService) http.Handler {
	gw := &GatewayConfig{
		DisableLogging: true,
		IAMService:     svc,
		Credentials:    credverify.New(svc, nil),
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		ctx = reqctx.WithService(ctx, "iam")
		ctx = withPrincipalField(ctx, fieldAccountID, "000000000000")
		ctx = withPrincipalField(ctx, fieldIdentity, "root")
		ctx = withPrincipalField(ctx, fieldPrincipalType, gwfix.PrincipalTypeUser)
		r = r.WithContext(ctx)
		if err := gw.IAM_Request(w, r); err != nil {
			gw.ErrorHandler(w, r, err)
		}
	})
}

func unmarshalIAMExample(t *testing.T, action, field string, data json.RawMessage, target any) {
	t.Helper()

	if len(data) == 0 || string(data) == "null" {
		return
	}
	require.NoError(t, json.Unmarshal(data, target), "%s example %s", action, field)
}

func normalizeEmptySlices(v reflect.Value) {
	if !v.IsValid() {
		return
	}
	if v.Kind() == reflect.Interface && !v.IsNil() {
		v = v.Elem()
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		normalizeEmptySlices(v.Elem())
		return
	}
	if v.Kind() != reflect.Struct {
		return
	}

	for _, field := range v.Fields() {
		switch field.Kind() {
		case reflect.Slice:
			if field.Len() == 0 && field.CanSet() {
				field.Set(reflect.Zero(field.Type()))
			}
			for j := 0; j < field.Len(); j++ {
				normalizeEmptySlices(field.Index(j))
			}
		case reflect.Pointer, reflect.Struct, reflect.Interface:
			normalizeEmptySlices(field)
		}
	}
}

// realAliasesIAMService answers ListAccountAliases from the real service, which
// needs no storage, so the test sees the response the gateway actually sends.
type realAliasesIAMService struct {
	flexMockIAMService
}

func (realAliasesIAMService) ListAccountAliases(accountID string, input *iam.ListAccountAliasesInput) (*iam.ListAccountAliasesOutput, error) {
	return (&iamdomain.IAMServiceImpl{}).ListAccountAliases(accountID, input)
}

type iamExamplesFile struct {
	Examples map[string][]iamExample `json:"examples"`
}

func echoIAMCall[In any, Out any](svc *echoIAMService, accountID string, input *In) (*Out, error) {
	svc.gotAccountID = accountID
	svc.gotInput = input

	if svc.wantOutput == nil {
		return new(Out), nil
	}
	output, ok := svc.wantOutput.(*Out)
	if !ok {
		return nil, fmt.Errorf("echo IAM output type %T is not %T", svc.wantOutput, new(Out))
	}
	return output, nil
}

type iamExample struct {
	Input  json.RawMessage `json:"input"`
	Output json.RawMessage `json:"output"`
	Title  string          `json:"title"`
}

// stsRegistration is the STS registration over the IAMService and STSService
// gw holds, as the awsgw role composes it. Set them on gw first.
func stsRegistration(gw *GatewayConfig) dispatch.Registration {
	return stsawsapi.NewRegistration(stsawsapi.Deps{STS: gw.STSService, IAM: gw.IAMService})
}

// withIAMAndSTS registers IAM and STS together on gw, with the unsigned
// web-identity capability, as awsgw composes them.
func withIAMAndSTS(gw *GatewayConfig) *GatewayConfig {
	gw.AnonymousSTS = stsawsapi.NewAnonymousAssumeRoleWithWebIdentity(stsawsapi.Deps{STS: gw.STSService, IAM: gw.IAMService})
	return withServices(gw, iamRegistration(gw), stsRegistration(gw))
}

// withSTS registers STS alone on gw and composes its unsigned web-identity
// capability, as awsgw does.
func withSTS(gw *GatewayConfig) *GatewayConfig {
	gw.AnonymousSTS = stsawsapi.NewAnonymousAssumeRoleWithWebIdentity(stsawsapi.Deps{STS: gw.STSService, IAM: gw.IAMService})
	return withServices(gw, stsRegistration(gw))
}

// STS_Request dispatches r to the registered STS service exactly as Request
// does, registering it first from the fields gw holds when the test has not.
func (gw *GatewayConfig) STS_Request(w http.ResponseWriter, r *http.Request) error {
	if gw.Services == nil {
		withSTS(gw)
	}
	entry, ok := gw.registered(stsawsapi.ServiceName)
	if !ok {
		panic("STS is not registered on this gateway")
	}
	return gw.dispatchRegistered(entry, w, r)
}

// flexMockSTSService is a configurable STSService mock with per-method
// overrides. It embeds the interface so it satisfies the full contract; only
// the fn-field methods below are wired, and any other method nil-panics if a
// test reaches an unmocked path.
type flexMockSTSService struct {
	stsdomain.STSService

	assumeRoleFn        func(string, string, string, *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error)
	getCallerIdentityFn func(string, string, string, *sts.GetCallerIdentityInput) (*sts.GetCallerIdentityOutput, error)
	getSessionTokenFn   func(string, string, string, string, *sts.GetSessionTokenInput) (*sts.GetSessionTokenOutput, error)
	lookupSessionFn     func(string) (*stsdomain.SessionCredential, error)
	assumeWebIdentityFn func(*sts.AssumeRoleWithWebIdentityInput) (*sts.AssumeRoleWithWebIdentityOutput, error)
}

func (m *flexMockSTSService) AssumeRole(callerAccountID, callerARN, callerIdentity string, input *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error) {
	if m.assumeRoleFn != nil {
		return m.assumeRoleFn(callerAccountID, callerARN, callerIdentity, input)
	}
	return &sts.AssumeRoleOutput{}, nil
}

func (m *flexMockSTSService) AssumeRoleWithWebIdentity(input *sts.AssumeRoleWithWebIdentityInput) (*sts.AssumeRoleWithWebIdentityOutput, error) {
	if m.assumeWebIdentityFn != nil {
		return m.assumeWebIdentityFn(input)
	}
	return nil, errors.New(awserrors.ErrorNotImplemented)
}

func (m *flexMockSTSService) GetCallerIdentity(callerAccountID, callerARN, callerUserID string, input *sts.GetCallerIdentityInput) (*sts.GetCallerIdentityOutput, error) {
	if m.getCallerIdentityFn != nil {
		return m.getCallerIdentityFn(callerAccountID, callerARN, callerUserID, input)
	}
	return &sts.GetCallerIdentityOutput{
		Account: aws.String(callerAccountID),
		Arn:     aws.String(callerARN),
		UserId:  aws.String(callerUserID),
	}, nil
}

func (m *flexMockSTSService) GetSessionToken(callerAccountID, callerUserName, callerPrincipalType, callerAccessKeyID string, input *sts.GetSessionTokenInput) (*sts.GetSessionTokenOutput, error) {
	if m.getSessionTokenFn != nil {
		return m.getSessionTokenFn(callerAccountID, callerUserName, callerPrincipalType, callerAccessKeyID, input)
	}
	return &sts.GetSessionTokenOutput{}, nil
}

func (m *flexMockSTSService) LookupSessionCredential(akid string) (*stsdomain.SessionCredential, error) {
	if m.lookupSessionFn != nil {
		return m.lookupSessionFn(akid)
	}
	return nil, nil
}

const (
	stsTestRoleName = "app"
	stsTestRoleARN  = "arn:aws:iam::000000000000:role/app"
)

// stsRequestParams holds identity values the STS dispatcher reads from SigV4 context.
type stsRequestParams struct {
	accountID      string
	identity       string
	principalType  string
	accessKey      string
	assumedRoleARN string
	assumedRoleID  string
	// underlyingRoleARN is what the policy gate resolves an assumed-role
	// caller's policies by; SessionName never participates.
	underlyingRoleARN string
	stsSvc            stsdomain.STSService
	iamSvc            iamdomain.IAMService
}

// stsIdentityPolicy builds an IAM service returning the given statements as the
// caller's only identity policy, for a user and for an assumed role alike. The
// one stored role is the pathless `app`; withRoles replaces the set.
func stsIdentityPolicy(statements ...iamdomain.Statement) *stsPolicyIAMService {
	docs := []iamdomain.PolicyDocument{{Version: "2012-10-17", Statement: statements}}
	svc := &stsPolicyIAMService{roles: map[string]string{stsTestRoleName: stsTestRoleARN}}
	svc.GetUserPoliciesFn = func(_, _ string) ([]iamdomain.PolicyDocument, error) {
		return docs, nil
	}
	svc.GetRolePoliciesFn = func(_, _ string) ([]iamdomain.PolicyDocument, error) {
		return docs, nil
	}
	return svc
}

// stsStatement is shorthand for a single-action, single-resource statement.
func stsStatement(effect, action, resource string) iamdomain.Statement {
	return iamdomain.Statement{
		Effect:   effect,
		Action:   iamdomain.StringOrArr{action},
		Resource: iamdomain.StringOrArr{resource},
	}
}

func stsPolicyRequestAs(t *testing.T, p stsRequestParams, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return doRequest(setupSTSRequestHandler(p), req)
}

// deniedSTSService fails the test if any gated handler is reached, so a passing
// denial can only have come from the gate.
func deniedSTSService(t *testing.T) *flexMockSTSService {
	t.Helper()
	return &flexMockSTSService{
		assumeRoleFn: func(string, string, string, *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error) {
			t.Fatal("handler reached: the identity-policy gate should have denied this request")
			return nil, nil
		},
	}
}

func assertSTSAccessDenied(t *testing.T, resp *http.Response) string {
	t.Helper()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	b, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(b), "AccessDenied")
	return string(b)
}

// stsPolicyRequest dispatches an STS action as alice with the given identity
// policies.
func stsPolicyRequest(t *testing.T, iamSvc iamdomain.IAMService, stsSvc *flexMockSTSService, body string) *http.Response {
	t.Helper()
	if stsSvc == nil {
		stsSvc = deniedSTSService(t)
	}
	return stsPolicyRequestAs(t, stsRequestParams{
		accountID:     awsidentifiers.GlobalAccountID,
		identity:      "alice",
		principalType: gwfix.PrincipalTypeUser,
		accessKey:     "AKIAEXAMPLE",
		stsSvc:        stsSvc,
		iamSvc:        iamSvc,
	}, body)
}

// stripRequestID removes the per-request UUID so two error bodies can be
// compared for everything else.
func stripRequestID(body string) string {
	start := strings.Index(body, "<RequestId>")
	end := strings.Index(body, "</RequestId>")
	if start < 0 || end < start {
		return body
	}
	return body[:start] + body[end:]
}

func setupSTSRequestHandler(p stsRequestParams) http.Handler {
	if p.iamSvc == nil {
		p.iamSvc = &flexMockIAMService{}
	}
	gw := withVerifier(&GatewayConfig{
		DisableLogging: true,
		STSService:     p.stsSvc,
		IAMService:     p.iamSvc,
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		ctx = reqctx.WithService(ctx, "sts")
		ctx = withPrincipalField(ctx, fieldAccountID, p.accountID)
		ctx = withPrincipalField(ctx, fieldIdentity, p.identity)
		ctx = withPrincipalField(ctx, fieldPrincipalType, p.principalType)
		ctx = withPrincipalField(ctx, fieldAccessKey, p.accessKey)
		if p.assumedRoleARN != "" {
			ctx = withPrincipalField(ctx, fieldAssumedRoleARN, p.assumedRoleARN)
		}
		if p.assumedRoleID != "" {
			ctx = withPrincipalField(ctx, fieldAssumedRoleID, p.assumedRoleID)
			// The door seals aws:userid as the assumed-role ID for a role session.
			ctx = withPrincipalField(ctx, fieldUserID, p.assumedRoleID)
		}
		if p.underlyingRoleARN != "" {
			ctx = withPrincipalField(ctx, fieldUnderlyingRoleARN, p.underlyingRoleARN)
		}
		r = r.WithContext(ctx)
		if err := gw.STS_Request(w, r); err != nil {
			gw.ErrorHandler(w, r, err)
		}
	})
}

// stsPolicyIAMService serves the identity policies the STS gate evaluates, the
// GetUser lookup GetCallerIdentity performs, and the GetRole the gate resolves
// the target role with.
type stsPolicyIAMService struct {
	gwfix.PolicyIAM

	roles map[string]string
}

func (m *stsPolicyIAMService) GetRole(_ string, input *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	arn, ok := m.roles[aws.StringValue(input.RoleName)]
	if !ok {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return &iam.GetRoleOutput{Role: &iam.Role{
		RoleName: input.RoleName,
		Arn:      aws.String(arn),
	}}, nil
}

func (m *stsPolicyIAMService) GetUser(_ string, _ *iam.GetUserInput) (*iam.GetUserOutput, error) {
	return &iam.GetUserOutput{User: &iam.User{UserId: aws.String("AIDAALICE000")}}, nil
}

// withRoles replaces the roles GetRole answers for, keyed by name and mapped to
// the ARN IAM stored — which is what a resource-scoped grant is matched against.
func (m *stsPolicyIAMService) withRoles(roles map[string]string) *stsPolicyIAMService {
	m.roles = roles
	return m
}

func allowAllTaggingDocs() []iamdomain.PolicyDocument {
	return []iamdomain.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []iamdomain.Statement{
			{Effect: "Allow", Action: iamdomain.StringOrArr{"*"}, Resource: iamdomain.StringOrArr{"*"}},
		},
	}}
}

// taggingRegistration is the Tagging registration over agg.
func taggingRegistration(agg *tagging.Aggregator) dispatch.Registration {
	return taggingawsapi.NewRegistration(taggingawsapi.Deps{Aggregator: agg})
}

const taggingTargetPrefix = "ResourceGroupsTaggingAPI_20170126."

// withTagging registers the Resource Groups Tagging API on gw backed by agg, as
// the awsgw role composes it.
func withTagging(gw *GatewayConfig, agg *tagging.Aggregator) *GatewayConfig {
	return withServices(gw, taggingRegistration(agg))
}

// taggingPipelineApp wires SigV4AuthMiddleware -> Request as SetupRoutes does,
// on live test NATS, with docs as every principal's policy.
func taggingPipelineApp(t *testing.T, docs []iamdomain.PolicyDocument, agg *tagging.Aggregator) (*GatewayConfig, http.Handler) {
	t.Helper()
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	require.NoError(t, err)
	_, nc := testutil.StartTestNATS(t)

	gw := withVerifier(&GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
		NATSConn:       nc,
		IAMService: &gwfix.PolicyIAM{
			IAM: gwfix.IAM{
				MasterKey: gwfix.MasterKey,
				AccessKeys: map[string]*iamdomain.AccessKey{
					gwfix.AccessKey: {
						AccessKeyID:     gwfix.AccessKey,
						SecretAccessKey: encryptedSecret,
						UserName:        "alice",
						AccountID:       authzAccountID,
						Status:          "Active",
					},
				},
			},
			GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) { return docs, nil },
		},
	})
	withTagging(gw, agg)

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", gw.Request)
	return gw, r
}

// signTaggingRequest builds a POST / signed with credential-scope service
// "tagging", the shape every Resource Groups Tagging API client sends.
func signTaggingRequest(t *testing.T, target string, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Host = "localhost:9999"
	if target != "" {
		req.Header.Set("X-Amz-Target", target)
	}
	gwfix.SignRequestAs(t, req, body, gwfix.AccessKey, gwfix.SecretKey, "tagging", time.Now().UTC())
	return req
}

// newTaggingCBGateway serves ec2.CreateTags from a real TagsServiceImpl
// over a fresh JetStream bucket, with no legacy object store.
func newTaggingCBGateway(t *testing.T) (*GatewayConfig, *ec2tags.TagsServiceImpl) {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)
	kv, err := ec2tags.GetOrCreateTagsBucket(t.Context(), js)
	require.NoError(t, err)
	svc := ec2tags.NewTagsServiceImplWithStore(&config.Config{}, nil, kv)

	serve := func(subject string, fn func(*nats.Msg)) {
		sub, err := nc.Subscribe(subject, fn)
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
	serve("ec2.CreateTags", func(msg *nats.Msg) {
		natsmsg.ServeNATSRequestCtx(msg, func(ctx context.Context, in *ec2.CreateTagsInput) (*ec2.CreateTagsOutput, error) {
			return svc.CreateTags(ctx, in, natsmsg.AccountIDFromMsg(msg))
		})
	})

	gw := withVerifier(&GatewayConfig{DisableLogging: true, Region: authzRegion, IAMService: allowAllIAMService(), NATSConn: nc})
	return gw, svc
}

func requireEC2OK(t *testing.T, gw *GatewayConfig, body string) {
	t.Helper()
	w := serveEC2(gw, body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// ec2TagQuery builds a CreateTags or DeleteTags query body.
func ec2TagQuery(action string, resources []string, tags map[string]string) string {
	q := url.Values{"Action": {action}}
	for i, r := range resources {
		q.Set(fmt.Sprintf("ResourceId.%d", i+1), r)
	}
	i := 0
	for k, v := range tags {
		i++
		q.Set(fmt.Sprintf("Tag.%d.Key", i), k)
		q.Set(fmt.Sprintf("Tag.%d.Value", i), v)
	}
	return q.Encode()
}

func centralTagCount(t *testing.T, svc *ec2tags.TagsServiceImpl, resourceID string) int {
	t.Helper()
	out, err := svc.DescribeTags(t.Context(), &ec2.DescribeTagsInput{
		Filters: []*ec2.Filter{{Name: aws.String("resource-id"), Values: []*string{aws.String(resourceID)}}},
	}, authzAccountID)
	require.NoError(t, err)
	return len(out.Tags)
}

// operatorPipelineApp wires SigV4AuthMiddleware -> the two operator routes as
// SetupRoutes does, on live test NATS, with docs as every principal's policy.
// accountID owns the signing key; the region is gwfix.Region because
// gwfix.SignRequestAs always signs for it.
func operatorPipelineApp(t *testing.T, accountID string, docs []iamdomain.PolicyDocument, mutate func(*GatewayConfig)) (*GatewayConfig, http.Handler) {
	t.Helper()
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	require.NoError(t, err)
	_, nc := testutil.StartTestNATS(t)

	gw := withVerifier(&GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
		NATSConn:       nc,
		Version:        "v9.9.9-test",
		Commit:         "abc1234",
		IAMService: &gwfix.PolicyIAM{
			IAM: gwfix.IAM{
				MasterKey: gwfix.MasterKey,
				AccessKeys: map[string]*iamdomain.AccessKey{
					gwfix.AccessKey: {
						AccessKeyID:     gwfix.AccessKey,
						SecretAccessKey: encryptedSecret,
						UserName:        "admin",
						AccountID:       accountID,
						Status:          "Active",
					},
				},
			},
			GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) { return docs, nil },
		},
	})
	gw.IAMService = operatorIAM{gw.IAMService.(*gwfix.PolicyIAM)}
	if mutate != nil {
		mutate(gw)
	}
	withOperator(gw)

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.Use(traceActionEnricher)
	r.HandleFunc("/admin/{method}", gw.Admin_Request)
	r.HandleFunc("/*", gw.Request)
	return gw, r
}

func allowAllOperatorDocs() []iamdomain.PolicyDocument {
	return []iamdomain.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []iamdomain.Statement{
			{Effect: "Allow", Action: iamdomain.StringOrArr{"*"}, Resource: iamdomain.StringOrArr{"*"}},
		},
	}}
}

func adminCall(t *testing.T, app http.Handler, method, name, service string, body string) *http.Response {
	t.Helper()
	return doRequest(app, signOperatorRequest(t, method, "/admin/"+name, service, []byte(body)))
}

// operatorIAM adds the account listing /admin/ListAccounts reads to the policy
// mock, which leaves it to a nil embedded interface.
type operatorIAM struct {
	*gwfix.PolicyIAM
}

func (operatorIAM) ListAccounts() ([]*iamdomain.Account, error) {
	return []*iamdomain.Account{
		{AccountID: "000000000001", AccountName: "spinifex", Status: "ACTIVE", CreatedAt: "2026-01-02T03:04:05Z"},
		{AccountID: "000000000002", AccountName: "tenant", Status: "ACTIVE", CreatedAt: "2026-02-03T04:05:06Z"},
	}, nil
}

// signOperatorRequest builds a signed request for the spinifex scope (or any
// scope named), the shape the CLI and the operator tooling send.
func signOperatorRequest(t *testing.T, method, path, service string, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Host = "localhost:9999"
	gwfix.SignRequestAs(t, req, body, gwfix.AccessKey, gwfix.SecretKey, service, time.Now().UTC())
	return req
}

// adminError decodes the /admin JSON envelope and checks the request ID is on
// both the header and the body, which is how a client ties a failure to a log line.
func adminError(t *testing.T, resp *http.Response) operatorAdminError {
	t.Helper()
	require.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	var body operatorAdminError
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.NotEmpty(t, body.RequestID)
	require.Equal(t, body.RequestID, resp.Header.Get("X-Amzn-Requestid"))
	return body
}

func opBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(raw)
}

// spinifexAdminActions is the set of super-admin Query actions the spinifex
// registration serves, which the characterization tests walk.
var spinifexAdminActions = func() map[string]bool {
	m := map[string]bool{}
	for _, name := range operatoradmin.AdminActionNames() {
		m[name] = true
	}
	return m
}()

// withOperator registers the spinifex Query service on gw from the fields gw
// holds, as awsgw composes it.
func withOperator(gw *GatewayConfig) *GatewayConfig {
	b := dispatch.NewBuilder()
	deps := operatoradmin.Deps{
		Version:     gw.Version,
		Commit:      gw.Commit,
		NATS:        gw.NATSConn,
		RootCAs:     gw.RootCAs,
		ActiveNodes: gw.DiscoverActiveNodes,

		IAM:           gw.IAMService,
		Quota:         quotaFor(gw),
		ExpectedNodes: gw.ExpectedNodes,
	}
	reg := operatoradmin.NewRegistration(deps)
	if err := b.Register(reg); err != nil {
		panic(err)
	}
	if err := b.Register(ec2Registration(gw)); err != nil {
		panic(err)
	}
	gw.Services = b.Build()
	return gw
}

func spinifexQuery(t *testing.T, action string, args url.Values) *http.Request {
	t.Helper()
	if args == nil {
		args = url.Values{}
	}
	if action != "" {
		args.Set("Action", action)
	}
	body := []byte(args.Encode())
	req := signOperatorRequest(t, http.MethodPost, "/", "spinifex", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

// ec2ErrorCode reads the code out of the EC2 <Response><Errors> envelope the
// spinifex Query service renders its failures in.
func ec2ErrorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var body struct {
		Code      string `xml:"Errors>Error>Code"`
		RequestID string `xml:"RequestID"`
	}
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, xml.Unmarshal(raw, &body), string(raw))
	require.NotEmpty(t, body.RequestID, "the EC2 envelope carries its request ID in the body")
	return body.Code
}

const (
	streamAccount  = "000000000002"
	streamInstance = "i-0123456789abcdef0"
)

func instanceMetricsDocs(resource string) []iamdomain.PolicyDocument {
	return []iamdomain.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []iamdomain.Statement{
			{Effect: "Allow", Action: iamdomain.StringOrArr{"spinifex:GetInstanceMetrics"}, Resource: iamdomain.StringOrArr{resource}},
		},
	}}
}

// answerWatches replies to the instance's watch with one sample, as the
// owning node's collector would.
func answerWatches(t *testing.T, nc *nats.Conn) {
	t.Helper()
	ring, err := json.Marshal(telemetryv1.LiveRing{Samples: []telemetryv1.LiveSample{{TimeMillis: 1000, CPUPercent: 7}}})
	require.NoError(t, err)
	sub, err := nc.Subscribe(telemetryv1.LiveWatchSubject(streamAccount, streamInstance), func(msg *nats.Msg) {
		if msg.Reply != "" {
			_ = msg.Respond(ring)
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

// postSignedMetrics sends a signed GetInstanceMetrics over real HTTP, so the
// response can be read while the stream is still open.
func postSignedMetrics(t *testing.T, srv *httptest.Server) *http.Response {
	t.Helper()
	body := []byte(url.Values{"Action": {"GetInstanceMetrics"}, "InstanceId": {streamInstance}}.Encode())
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/", bytes.NewReader(body))
	require.NoError(t, err)
	req.Host = "localhost:9999"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	gwfix.SignRequestAs(t, req, body, gwfix.AccessKey, gwfix.SecretKey, "spinifex", time.Now().UTC())
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

type operatorAdminError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	RequestID string `json:"requestId"`
}

// allowAllECRDocs is the policy ecrPipelineApp uses when the test is not
// exercising authorization itself.
func allowAllECRDocs() []iamdomain.PolicyDocument {
	return []iamdomain.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []iamdomain.Statement{
			{Effect: "Allow", Action: iamdomain.StringOrArr{"*"}, Resource: iamdomain.StringOrArr{"*"}},
		},
	}}
}

const (
	ecrTestRegion   = "ap-southeast-2"
	ecrTestSuffix   = "spinifex.internal"
	ecrTestAudience = "ecr." + ecrTestRegion + "." + ecrTestSuffix
	ecrTestAccount  = "000000000001"
)

func decodeJSONError(t *testing.T, resp *http.Response) jsonErrorEnvelope {
	t.Helper()
	var out jsonErrorEnvelope
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

// ecrRegistrationInventory is the inventory the ECR registration declares to
// a registry, keyed as AWSOperationInventory expects.
func ecrRegistrationInventory() map[string]dispatch.Inventory {
	inventory := withECR(&GatewayConfig{}, awsapi.Deps{}).Services.Inventory()
	maps.Copy(inventory, withContainers(&GatewayConfig{}).Services.Inventory())
	return inventory
}

// serveECR dispatches r to gw's registered ECR service exactly as Request does
// past its NATS gate, first registering one with no capabilities if needed.
func (gw *GatewayConfig) serveECR(w http.ResponseWriter, r *http.Request) error {
	entry, ok := gw.registered(awsapi.ServiceName)
	if !ok {
		withECR(gw, awsapi.Deps{})
		entry, _ = gw.registered(awsapi.ServiceName)
	}
	return gw.dispatchRegistered(entry, w, r)
}

// jsonErrorEnvelope is the AWS JSON 1.1 error body shape.
type jsonErrorEnvelope struct {
	Type    string `json:"__type"`
	Message string `json:"message"`
}

// withECR registers the ECR control plane on gw backed by deps, as the awsgw
// role composes it. An unset deps.NATS takes gw.NATSConn.
func withECR(gw *GatewayConfig, deps awsapi.Deps) *GatewayConfig {
	if deps.NATS == nil {
		deps.NATS = gw.NATSConn
	}
	b := dispatch.NewBuilder()
	if err := b.Register(awsapi.NewRegistration(deps)); err != nil {
		panic(err)
	}
	gw.Services = b.Build()
	return gw
}

// ecrPipelineApp wires SigV4AuthMiddleware -> Request as SetupRoutes does, on
// live test NATS so Request dispatches. docs is every principal's policy; the
// region is gwfix.Region because gwfix.SignRequestAs always signs for it.
func ecrPipelineApp(t *testing.T, docs []iamdomain.PolicyDocument) (*GatewayConfig, http.Handler) {
	t.Helper()
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	require.NoError(t, err)
	_, nc := testutil.StartTestNATS(t)

	gw := withVerifier(&GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
		InternalSuffix: ecrTestSuffix,
		NATSConn:       nc,
		IAMService: &gwfix.PolicyIAM{
			IAM: gwfix.IAM{
				MasterKey: gwfix.MasterKey,
				AccessKeys: map[string]*iamdomain.AccessKey{
					gwfix.AccessKey: {
						AccessKeyID:     gwfix.AccessKey,
						SecretAccessKey: encryptedSecret,
						UserName:        "alice",
						AccountID:       ecrTestAccount,
						Status:          "Active",
					},
				},
			},
			GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) { return docs, nil },
		},
	})

	withECR(gw, awsapi.Deps{})

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", gw.Request)
	return gw, r
}

// signECRRequest builds a POST / signed with credential-scope service "ecr",
// the shape every ECR JSON 1.1 client sends.
func signECRRequest(t *testing.T, target string, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Host = "localhost:9999"
	if target != "" {
		req.Header.Set("X-Amz-Target", target)
	}
	gwfix.SignRequestAs(t, req, body, gwfix.AccessKey, gwfix.SecretKey, "ecr", time.Now().UTC())
	return req
}

// newECRAuth builds a wired issuer+verifier over a freshly generated signing key.
func newECRAuth(t *testing.T) (*ecrauth.Issuer, *ecrauth.Verifier) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	key, verify, err := ecrauth.LoadOrCreateSigningKey(t.Context(), js, ecrTestMasterKey(t))
	require.NoError(t, err)
	return ecrauth.NewIssuer(key, ecrTestAudience), ecrauth.NewVerifier(verify, ecrTestAudience)
}

// decodeAuthToken decodes an ECR authorizationToken ("AWS:<jwt>") to the jwt.
func decodeAuthToken(token string) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return "", err
	}
	_, jwtStr, _ := strings.Cut(string(decoded), ":")
	return jwtStr, nil
}

func newECRMockIAMService() *ecrMockIAMService {
	return &ecrMockIAMService{
		accessKeys:   map[string]*iamdomain.AccessKey{},
		users:        map[string]*iam.User{},
		roles:        map[string]*iam.Role{},
		accounts:     map[string]*iamdomain.Account{},
		userPolicies: map[string][]iamdomain.PolicyDocument{},
		rolePolicies: map[string][]iamdomain.PolicyDocument{},
	}
}

func seedECRTestUser(iamSvc *ecrMockIAMService, accountID, userName, akid string) {
	iamSvc.accessKeys[akid] = &iamdomain.AccessKey{
		AccessKeyID: akid,
		UserName:    userName,
		AccountID:   accountID,
		Status:      iamdomain.AccessKeyStatusActive,
	}
	iamSvc.users[accountID+"|"+userName] = &iam.User{
		UserName: aws.String(userName),
		UserId:   aws.String("AIDA" + strings.ToUpper(userName)),
		Arn:      aws.String("arn:aws:iam::" + accountID + ":user/" + userName),
	}
	iamSvc.accounts[accountID] = &iamdomain.Account{AccountID: accountID, Status: iamdomain.AccountStatusActive}
}

const (
	ecrPrincipalTestAccount = "000000000042"
	ecrPrincipalTestAKID    = "AKIATESTLONGLIVED0001"
	ecrPrincipalTestASID    = "ASIATESTSESSION000001"
)

const ecrPathedUserARN = "arn:aws:iam::" + ecrPrincipalTestAccount + ":user/eng/dev"

func ecrTestMasterKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	_, err := rand.Read(k)
	require.NoError(t, err)
	return k
}

// ecrMockIAMService implements iamdomain.IAMService for ECR principal
// rehydration tests. Only the methods resolveECRPrincipal calls are wired;
// any other method nil-panics on the embedded nil interface, surfacing an
// unexpected call rather than silently returning a zero value.
type ecrMockIAMService struct {
	iamdomain.IAMService

	accessKeys   map[string]*iamdomain.AccessKey
	users        map[string]*iam.User
	roles        map[string]*iam.Role
	accounts     map[string]*iamdomain.Account
	userPolicies map[string][]iamdomain.PolicyDocument
	rolePolicies map[string][]iamdomain.PolicyDocument

	// genericErr, when set, is returned by LookupAccessKey/GetUser/GetRole/
	// GetAccount/GetUserPolicies/GetRolePolicies instead of the usual
	// not-found error, simulating an IAM backend outage rather than a
	// missing record.
	genericErr error

	// userPolicyErr fails only the policy read, so a test can let the principal
	// rehydrate and then break authorization.
	userPolicyErr error
}

func (m *ecrMockIAMService) GetAccount(accountID string) (*iamdomain.Account, error) {
	if m.genericErr != nil {
		return nil, m.genericErr
	}
	a, ok := m.accounts[accountID]
	if !ok {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return a, nil
}

func (m *ecrMockIAMService) GetRole(accountID string, input *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	if m.genericErr != nil {
		return nil, m.genericErr
	}
	r, ok := m.roles[accountID+"|"+aws.StringValue(input.RoleName)]
	if !ok {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return &iam.GetRoleOutput{Role: r}, nil
}

func (m *ecrMockIAMService) GetRolePolicies(accountID, roleName string) ([]iamdomain.PolicyDocument, error) {
	if m.genericErr != nil {
		return nil, m.genericErr
	}
	return m.rolePolicies[accountID+"|"+roleName], nil
}

func (m *ecrMockIAMService) GetUser(accountID string, input *iam.GetUserInput) (*iam.GetUserOutput, error) {
	if m.genericErr != nil {
		return nil, m.genericErr
	}
	u, ok := m.users[accountID+"|"+aws.StringValue(input.UserName)]
	if !ok {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return &iam.GetUserOutput{User: u}, nil
}

func (m *ecrMockIAMService) GetUserPolicies(accountID, userName string) ([]iamdomain.PolicyDocument, error) {
	if m.genericErr != nil {
		return nil, m.genericErr
	}
	if m.userPolicyErr != nil {
		return nil, m.userPolicyErr
	}
	return m.userPolicies[accountID+"|"+userName], nil
}

func (m *ecrMockIAMService) LookupAccessKey(accessKeyID string) (*iamdomain.AccessKey, error) {
	if m.genericErr != nil {
		return nil, m.genericErr
	}
	ak, ok := m.accessKeys[accessKeyID]
	if !ok {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return ak, nil
}
