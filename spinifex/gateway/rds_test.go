package gateway

import (
	"context"
	"errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/internal/testkit"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	rdsinstance "github.com/mulgadc/spinifex/spinifex/domains/rds/instance"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const rdsTestAccountID = "123456789012"

func setupRDSRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	ctx := reqctx.WithService(req.Context(), "rds")
	ctx = withPrincipalField(ctx, fieldAccountID, rdsTestAccountID)
	return withTestIdentity(req.WithContext(ctx))
}

// A signed customer request: an IAM user with a resolvable identity, which is
// what turns the policy check from the pre-IAM bypass into a real evaluation.
func setupRDSUserRequest(body, accountID string) *http.Request {
	req := setupRDSRequest(body)
	ctx := withPrincipalField(req.Context(), fieldAccountID, accountID)
	ctx = withPrincipalField(ctx, fieldIdentity, "alice")
	ctx = withPrincipalField(ctx, fieldPrincipalType, principalTypeUser)
	return req.WithContext(ctx)
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
		IAMService: &policyMockIAMService{
			getUserPoliciesFn: counted,
			getRolePoliciesFn: counted,
		},
	}), probe
}

func TestRDSRequest_MalformedScalarReturnsHTTP400(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	gw := withVerifier(&GatewayConfig{DisableLogging: true, NATSConn: nc, IAMService: allowAllIAMService()})
	for _, body := range []string{
		"Action=CreateDBInstance&AllocatedStorage=abc",
		"Action=CreateDBInstance&MultiAZ=yes",
	} {
		t.Run(body, func(t *testing.T) {
			req := setupRDSRequest(body)
			err := gw.serveRDS(httptest.NewRecorder(), req)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())

			w := httptest.NewRecorder()
			gw.ErrorHandler(w, req, err)
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Contains(t, w.Body.String(), "<Code>"+awserrors.ErrorInvalidParameterValue+"</Code>")
		})
	}
}

// RDS errors must use the IAM-style <ErrorResponse> envelope: the aws-sdk-go
// query unmarshaler rejects the EC2 <Response><Errors> shape for this service.
func TestRDSErrorHandler_UsesIAMEnvelope(t *testing.T) {
	gw := withVerifier(withRDS(&GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}))
	w := httptest.NewRecorder()
	gw.ErrorHandler(w, setupRDSRequest("Action=CreateDBInstance"), errNotImplementedForTest)

	body := w.Body.String()
	assert.Contains(t, body, "<ErrorResponse>")
	assert.Contains(t, body, "<Code>"+awserrors.ErrorNotImplemented+"</Code>")
}

func TestRDSErrorHandler_UsesRDSUnsupportedActionWording(t *testing.T) {
	gw := withVerifier(withRDS(&GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}))
	w := httptest.NewRecorder()
	gw.ErrorHandler(w, setupRDSRequest("Action=CreateDBCluster"),
		errors.New(awserrors.ErrorOperationNotSupported))

	body := w.Body.String()
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, body, "RDS")
	assert.Contains(t, body, "v1")
	assert.NotContains(t, body, "registry")
}

var errNotImplementedForTest = errors.New(awserrors.ErrorNotImplemented)

// TestRDSRequest_CreateDBInstance_EmptyMasterUserPasswordReturns400 runs the real public path:
// a CreateDBInstance query request through RDS_Request, over NATS, into the production
// rdsinstance.Service, and the resulting error through ErrorHandler's HTTP/XML rendering. AWS
// returns 400 InvalidParameterValue for a create with no MasterUserPassword.
func TestRDSRequest_CreateDBInstance_EmptyMasterUserPasswordReturns400(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	svc := rdsinstance.NewService(nc, "ap-southeast-2")
	sub, err := nc.Subscribe(rdsinstance.SubjectCreateDBInstance, func(msg *nats.Msg) {
		natsmsg.ServeNATSRequestCtx(msg, func(ctx context.Context, in *rds.CreateDBInstanceInput) (*rds.CreateDBInstanceOutput, error) {
			return svc.CreateDBInstance(ctx, in, natsmsg.AccountIDFromMsg(msg))
		})
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	gw := withVerifier(&GatewayConfig{DisableLogging: true, NATSConn: nc, IAMService: allowAllIAMService()})
	req := setupRDSRequest("Action=CreateDBInstance&DBInstanceIdentifier=orders-db&Engine=postgres&" +
		"DBInstanceClass=db.t3.medium&AllocatedStorage=20&MasterUsername=appuser&DBName=orders")

	reqErr := gw.serveRDS(httptest.NewRecorder(), req)
	require.Error(t, reqErr)

	w := httptest.NewRecorder()
	gw.ErrorHandler(w, req, reqErr)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "<Code>"+awserrors.ErrorInvalidParameterValue+"</Code>")
}
