package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const stsTestRoleARN = "arn:aws:iam::000000000000:role/app"

// stsPolicyIAMService serves the identity policies the STS gate evaluates plus
// the GetUser lookup GetCallerIdentity performs for user principals.
type stsPolicyIAMService struct {
	policyMockIAMService
}

func (m *stsPolicyIAMService) GetUser(_ string, _ *iam.GetUserInput) (*iam.GetUserOutput, error) {
	return &iam.GetUserOutput{User: &iam.User{UserId: aws.String("AIDAALICE000")}}, nil
}

// stsIdentityPolicy builds an IAM service returning the given statements as
// alice's only identity policy.
func stsIdentityPolicy(statements ...handlers_iam.Statement) *stsPolicyIAMService {
	docs := []handlers_iam.PolicyDocument{{Version: "2012-10-17", Statement: statements}}
	svc := &stsPolicyIAMService{}
	svc.getUserPoliciesFn = func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
		return docs, nil
	}
	return svc
}

// stsStatement is shorthand for a single-action, single-resource statement.
func stsStatement(effect, action, resource string) handlers_iam.Statement {
	return handlers_iam.Statement{
		Effect:   effect,
		Action:   handlers_iam.StringOrArr{action},
		Resource: handlers_iam.StringOrArr{resource},
	}
}

// stsPolicyRequest dispatches an STS action as alice with the given identity
// policies. The STS mock fails the test if the handler is reached, so callers
// expecting success must supply their own service.
func stsPolicyRequest(t *testing.T, iamSvc handlers_iam.IAMService, stsSvc *flexMockSTSService, body string) *http.Response {
	t.Helper()
	if stsSvc == nil {
		stsSvc = &flexMockSTSService{
			assumeRoleFn: func(string, string, string, *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error) {
				t.Fatal("handler reached: the identity-policy gate should have denied this request")
				return nil, nil
			},
			getSessionTokenFn: func(string, string, string, string, *sts.GetSessionTokenInput) (*sts.GetSessionTokenOutput, error) {
				t.Fatal("handler reached: the identity-policy gate should have denied this request")
				return nil, nil
			},
		}
	}
	handler := setupSTSRequestHandler(stsRequestParams{
		accountID:     utils.GlobalAccountID,
		identity:      "alice",
		principalType: principalTypeUser,
		accessKey:     "AKIAEXAMPLE",
		stsSvc:        stsSvc,
		iamSvc:        iamSvc,
	})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return doRequest(handler, req)
}

func assertAccessDenied(t *testing.T, resp *http.Response) {
	t.Helper()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	b, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(b), "AccessDenied")
}

// TestSTSRequest_AssumeRole_IdentityPolicyGate covers the identity-side gate.
// The STS mock would happily mint credentials, so a denial can only come from
// the policy check that now runs ahead of the role's trust policy.
func TestSTSRequest_AssumeRole_IdentityPolicyGate(t *testing.T) {
	body := "Action=AssumeRole&RoleArn=" + stsTestRoleARN + "&RoleSessionName=s1"

	t.Run("empty policy denied", func(t *testing.T) {
		resp := stsPolicyRequest(t, stsIdentityPolicy(), nil, body)
		assertAccessDenied(t, resp)
	})

	t.Run("explicit deny beats allow", func(t *testing.T) {
		svc := stsIdentityPolicy(
			stsStatement("Allow", "sts:*", "*"),
			stsStatement("Deny", "sts:AssumeRole", "*"),
		)
		resp := stsPolicyRequest(t, svc, nil, body)
		assertAccessDenied(t, resp)
	})

	t.Run("allowed principal reaches handler", func(t *testing.T) {
		called := false
		stsSvc := &flexMockSTSService{
			assumeRoleFn: func(string, string, string, *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error) {
				called = true
				return &sts.AssumeRoleOutput{
					Credentials: &sts.Credentials{AccessKeyId: aws.String("ASIAEXAMPLE123")},
				}, nil
			},
		}
		svc := stsIdentityPolicy(stsStatement("Allow", "sts:AssumeRole", "*"))
		resp := stsPolicyRequest(t, svc, stsSvc, body)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.True(t, called)
	})
}

// TestSTSRequest_AssumeRole_PolicyScopedToRole proves the gate evaluates the
// target role ARN, not "*", so a grant on one role does not open another.
func TestSTSRequest_AssumeRole_PolicyScopedToRole(t *testing.T) {
	svc := stsIdentityPolicy(stsStatement("Allow", "sts:AssumeRole", stsTestRoleARN))

	t.Run("granted role allowed", func(t *testing.T) {
		stsSvc := &flexMockSTSService{}
		resp := stsPolicyRequest(t, svc, stsSvc,
			"Action=AssumeRole&RoleArn="+stsTestRoleARN+"&RoleSessionName=s1")
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("other role denied", func(t *testing.T) {
		resp := stsPolicyRequest(t, svc, nil,
			"Action=AssumeRole&RoleArn=arn:aws:iam::000000000000:role/other&RoleSessionName=s1")
		assertAccessDenied(t, resp)
	})
}

// TestSTSRequest_AssumeRole_PathPrefixCannotWidenGrant pins the gate to the
// same role the handler resolves. Roles are keyed by account and name, so a
// caller-supplied path must not make a scoped grant match a different role.
func TestSTSRequest_AssumeRole_PathPrefixCannotWidenGrant(t *testing.T) {
	svc := stsIdentityPolicy(
		stsStatement("Allow", "sts:AssumeRole", "arn:aws:iam::000000000000:role/app-*"),
	)

	t.Run("path prefix does not reach another role", func(t *testing.T) {
		resp := stsPolicyRequest(t, svc, nil,
			"Action=AssumeRole&RoleArn=arn:aws:iam::000000000000:role/app-x/admin&RoleSessionName=s1")
		assertAccessDenied(t, resp)
	})

	t.Run("path is ignored on a granted role", func(t *testing.T) {
		resp := stsPolicyRequest(t, svc, &flexMockSTSService{},
			"Action=AssumeRole&RoleArn=arn:aws:iam::000000000000:role/team/app-worker&RoleSessionName=s1")
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("malformed ARN rejected before evaluation", func(t *testing.T) {
		resp := stsPolicyRequest(t, svc, nil, "Action=AssumeRole&RoleArn=not-an-arn&RoleSessionName=s1")
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		b, _ := io.ReadAll(resp.Body)
		assert.Contains(t, string(b), "ValidationError")
	})
}

// TestSTSRequest_GetSessionToken_NotGated pins the AWS contract: it is an
// authentication operation requiring no permission, so an explicit Deny on the
// identity policy does not stop it reaching the handler.
func TestSTSRequest_GetSessionToken_NotGated(t *testing.T) {
	svc := stsIdentityPolicy(stsStatement("Deny", "sts:*", "*"))

	called := false
	stsSvc := &flexMockSTSService{
		getSessionTokenFn: func(string, string, string, string, *sts.GetSessionTokenInput) (*sts.GetSessionTokenOutput, error) {
			called = true
			return &sts.GetSessionTokenOutput{
				Credentials: &sts.Credentials{AccessKeyId: aws.String("ASIAEXAMPLE123")},
			}, nil
		},
	}

	resp := stsPolicyRequest(t, svc, stsSvc, "Action=GetSessionToken&DurationSeconds=3600")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, called)
}

// TestSTSRequest_GetCallerIdentity_NotGated pins the AWS contract: the action
// requires no permissions and cannot be denied by policy.
func TestSTSRequest_GetCallerIdentity_NotGated(t *testing.T) {
	svc := stsIdentityPolicy(stsStatement("Deny", "sts:*", "*"))
	resp := stsPolicyRequest(t, svc, &flexMockSTSService{}, "Action=GetCallerIdentity")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	b, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(b), "<Arn>arn:aws:iam::000000000000:user/alice</Arn>")
}
