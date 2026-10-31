//test:in-package — drives EC2_Request through the gateway's unexported test
// helpers (setupEC2Request, policyMockIAMService) and auth context keys.

package gateway

import (
	"net/http/httptest"
	"testing"

	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	authzAccountID = "123456789012"
	authzRegion    = "ap-southeast-2"
)

// scopedPolicyGateway serves one policy document to every principal.
func scopedPolicyGateway(statements ...iamdomain.Statement) *GatewayConfig {
	docs := []iamdomain.PolicyDocument{{Version: "2012-10-17", Statement: statements}}
	return withVerifier(&GatewayConfig{
		DisableLogging: true,
		Region:         authzRegion,
		IAMService: &policyMockIAMService{
			getUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) { return docs, nil },
			getRolePoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) { return docs, nil },
		},
	})
}

func statement(effect, action, resource string) iamdomain.Statement {
	return iamdomain.Statement{
		Effect:   effect,
		Action:   iamdomain.StringOrArr{action},
		Resource: iamdomain.StringOrArr{resource},
	}
}

// dispatchEC2 drives the gateway with no NATS connection. A permitted request
// therefore reaches the handler and fails there, which is what proves the policy
// check ran ahead of the resource existing at all.
func dispatchEC2(t *testing.T, gw *GatewayConfig, body string) error {
	t.Helper()
	return gw.EC2_Request(httptest.NewRecorder(), setupEC2Request(body, authzAccountID))
}

// assertDenied matches on the resolved code, not the message: a policy denial
// names the principal, action and resource ahead of the code.
func assertDenied(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	code, ok := awserrors.ResolveErrorCode(err)
	require.True(t, ok, "unresolvable error code: %v", err)
	assert.Equal(t, awserrors.ErrorAccessDenied, code)
}

// assertUnauthorized asserts EC2's policy denial, UnauthorizedOperation.
func assertUnauthorized(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorUnauthorizedOperation, err.Error())
}

// assertNotDenied asserts the policy gate passed where the handler failure that
// follows is not fixed. It resolves the code for the same reason assertDenied
// does: the denial message is no longer the bare code.
func assertNotDenied(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	code, _ := awserrors.ResolveErrorCode(err)
	assert.NotEqual(t, awserrors.ErrorAccessDenied, code)
}

// assertPermitted asserts the policy gate passed. The request then fails on the
// absent NATS connection rather than on authorization, so a denial cannot hide
// behind a not-found.
func assertPermitted(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

// profileIAMService resolves every instance profile to one holding role app, or
// fails resolution with resolveErr.
type profileIAMService struct {
	policyMockIAMService

	resolveErr error
}

func (s *profileIAMService) ResolveInstanceProfile(accountID, _ string) (*iamdomain.InstanceProfile, error) {
	if s.resolveErr != nil {
		return nil, s.resolveErr
	}
	return &iamdomain.InstanceProfile{AccountID: accountID, RoleName: "app"}, nil
}

func (s *profileIAMService) CanonicalResourceARN(accountID string, _ arn.IAMResourceType, name string) (string, error) {
	return "arn:aws:iam::" + accountID + ":role/" + name, nil
}
