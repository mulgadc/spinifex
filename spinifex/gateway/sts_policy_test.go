package gateway

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	stsTestRoleName = "app"
	stsTestRoleARN  = "arn:aws:iam::000000000000:role/app"
)

// stsPolicyIAMService serves the identity policies the STS gate evaluates, the
// GetUser lookup GetCallerIdentity performs, and the GetRole the gate resolves
// the target role with.
type stsPolicyIAMService struct {
	policyMockIAMService

	roles map[string]string
}

func (m *stsPolicyIAMService) GetUser(_ string, _ *iam.GetUserInput) (*iam.GetUserOutput, error) {
	return &iam.GetUserOutput{User: &iam.User{UserId: aws.String("AIDAALICE000")}}, nil
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

// stsIdentityPolicy builds an IAM service returning the given statements as the
// caller's only identity policy, for a user and for an assumed role alike. The
// one stored role is the pathless `app`; withRoles replaces the set.
func stsIdentityPolicy(statements ...handlers_iam.Statement) *stsPolicyIAMService {
	docs := []handlers_iam.PolicyDocument{{Version: "2012-10-17", Statement: statements}}
	svc := &stsPolicyIAMService{roles: map[string]string{stsTestRoleName: stsTestRoleARN}}
	svc.getUserPoliciesFn = func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
		return docs, nil
	}
	svc.getRolePoliciesFn = func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
		return docs, nil
	}
	return svc
}

// withRoles replaces the roles GetRole answers for, keyed by name and mapped to
// the ARN IAM stored — which is what a resource-scoped grant is matched against.
func (m *stsPolicyIAMService) withRoles(roles map[string]string) *stsPolicyIAMService {
	m.roles = roles
	return m
}

// stsStatement is shorthand for a single-action, single-resource statement.
func stsStatement(effect, action, resource string) handlers_iam.Statement {
	return handlers_iam.Statement{
		Effect:   effect,
		Action:   handlers_iam.StringOrArr{action},
		Resource: handlers_iam.StringOrArr{resource},
	}
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

// stsPolicyRequest dispatches an STS action as alice with the given identity
// policies.
func stsPolicyRequest(t *testing.T, iamSvc handlers_iam.IAMService, stsSvc *flexMockSTSService, body string) *http.Response {
	t.Helper()
	if stsSvc == nil {
		stsSvc = deniedSTSService(t)
	}
	return stsPolicyRequestAs(t, stsRequestParams{
		accountID:     utils.GlobalAccountID,
		identity:      "alice",
		principalType: principalTypeUser,
		accessKey:     "AKIAEXAMPLE",
		stsSvc:        stsSvc,
		iamSvc:        iamSvc,
	}, body)
}

func stsPolicyRequestAs(t *testing.T, p stsRequestParams, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return doRequest(setupSTSRequestHandler(p), req)
}

func assertSTSAccessDenied(t *testing.T, resp *http.Response) string {
	t.Helper()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	b, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(b), "AccessDenied")
	return string(b)
}

// TestSTSRequest_AssumeRole_IdentityPolicyGate covers the identity-side gate.
// The STS mock would happily mint credentials, so a denial can only come from
// the policy check that now runs ahead of the role's trust policy.
func TestSTSRequest_AssumeRole_IdentityPolicyGate(t *testing.T) {
	body := "Action=AssumeRole&RoleArn=" + stsTestRoleARN + "&RoleSessionName=s1"

	t.Run("empty policy denied", func(t *testing.T) {
		assertSTSAccessDenied(t, stsPolicyRequest(t, stsIdentityPolicy(), nil, body))
	})

	t.Run("explicit deny beats allow", func(t *testing.T) {
		svc := stsIdentityPolicy(
			stsStatement("Allow", "sts:*", "*"),
			stsStatement("Deny", "sts:AssumeRole", "*"),
		)
		assertSTSAccessDenied(t, stsPolicyRequest(t, svc, nil, body))
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

// TestSTSRequest_AssumeRole_AssumedRoleCallerIsGated is the case that would
// have caught the ECS agent regression: a role session whose underlying role
// grants a non-sts action and nothing else must not be able to assume anything.
func TestSTSRequest_AssumeRole_AssumedRoleCallerIsGated(t *testing.T) {
	body := "Action=AssumeRole&RoleArn=" + stsTestRoleARN + "&RoleSessionName=s1"
	params := func(iamSvc handlers_iam.IAMService, stsSvc *flexMockSTSService) stsRequestParams {
		return stsRequestParams{
			accountID:         utils.GlobalAccountID,
			identity:          "s1",
			principalType:     principalTypeAssumedRole,
			accessKey:         "ASIAEXAMPLE",
			assumedRoleARN:    "arn:aws:sts::000000000000:assumed-role/ecsInstanceRole/s1",
			assumedRoleID:     "AROAECS:s1",
			underlyingRoleARN: "arn:aws:iam::000000000000:role/ecsInstanceRole",
			stsSvc:            stsSvc,
			iamSvc:            iamSvc,
		}
	}

	t.Run("ecs-only role denied", func(t *testing.T) {
		svc := stsIdentityPolicy(stsStatement("Allow", "ecs:*", "*"))
		resp := stsPolicyRequestAs(t, params(svc, deniedSTSService(t)), body)
		assertSTSAccessDenied(t, resp)
	})

	t.Run("role granted sts:AssumeRole reaches handler", func(t *testing.T) {
		svc := stsIdentityPolicy(
			stsStatement("Allow", "ecs:*", "*"),
			stsStatement("Allow", "sts:AssumeRole", "arn:aws:iam::000000000000:role/*"),
		)
		resp := stsPolicyRequestAs(t, params(svc, &flexMockSTSService{}), body)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})
}

// TestSTSRequest_AssumeRole_PolicyScopedToRole proves the gate evaluates the
// target role ARN, not "*", so a grant on one role does not open another.
func TestSTSRequest_AssumeRole_PolicyScopedToRole(t *testing.T) {
	roles := map[string]string{
		stsTestRoleName: stsTestRoleARN,
		"other":         "arn:aws:iam::000000000000:role/other",
	}

	t.Run("granted role allowed", func(t *testing.T) {
		svc := stsIdentityPolicy(stsStatement("Allow", "sts:AssumeRole", stsTestRoleARN)).withRoles(roles)
		resp := stsPolicyRequest(t, svc, &flexMockSTSService{},
			"Action=AssumeRole&RoleArn="+stsTestRoleARN+"&RoleSessionName=s1")
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("other role denied", func(t *testing.T) {
		svc := stsIdentityPolicy(stsStatement("Allow", "sts:AssumeRole", stsTestRoleARN)).withRoles(roles)
		resp := stsPolicyRequest(t, svc, nil,
			"Action=AssumeRole&RoleArn=arn:aws:iam::000000000000:role/other&RoleSessionName=s1")
		assertSTSAccessDenied(t, resp)
	})
}

// TestSTSRequest_AssumeRole_PathIsResolvedNotTrusted pins the gate to the role
// the handler resolves. Roles are keyed by account and name, so a path the
// caller invented must not make a scoped grant match a role it does not name,
// while a role genuinely stored under a path is matched by its full ARN.
func TestSTSRequest_AssumeRole_PathIsResolvedNotTrusted(t *testing.T) {
	t.Run("invented path does not reach another role", func(t *testing.T) {
		// `app-x/admin` resolves `admin`, whose stored ARN carries no path and
		// so falls outside the grant.
		svc := stsIdentityPolicy(
			stsStatement("Allow", "sts:AssumeRole", "arn:aws:iam::000000000000:role/app-*"),
		).withRoles(map[string]string{"admin": "arn:aws:iam::000000000000:role/admin"})
		resp := stsPolicyRequest(t, svc, nil,
			"Action=AssumeRole&RoleArn=arn:aws:iam::000000000000:role/app-x/admin&RoleSessionName=s1")
		assertSTSAccessDenied(t, resp)
	})

	t.Run("stored ARN matching exactly is admitted", func(t *testing.T) {
		svc := stsIdentityPolicy(
			stsStatement("Allow", "sts:AssumeRole", "arn:aws:iam::000000000000:role/app-*"),
		).withRoles(map[string]string{"app-worker": "arn:aws:iam::000000000000:role/app-worker"})
		resp := stsPolicyRequest(t, svc, &flexMockSTSService{},
			"Action=AssumeRole&RoleArn=arn:aws:iam::000000000000:role/app-worker&RoleSessionName=s1")
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("a pathed role matches a policy naming its full ARN", func(t *testing.T) {
		const pathed = "arn:aws:iam::000000000000:role/team/app-worker"
		svc := stsIdentityPolicy(stsStatement("Allow", "sts:AssumeRole", pathed)).
			withRoles(map[string]string{"app-worker": pathed})
		resp := stsPolicyRequest(t, svc, &flexMockSTSService{},
			"Action=AssumeRole&RoleArn="+pathed+"&RoleSessionName=s1")
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("malformed ARN rejected before evaluation", func(t *testing.T) {
		resp := stsPolicyRequest(t, stsIdentityPolicy(), nil,
			"Action=AssumeRole&RoleArn=not-an-arn&RoleSessionName=s1")
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		b, _ := io.ReadAll(resp.Body)
		assert.Contains(t, string(b), "ValidationError")
	})
}

// TestSTSRequest_AssumeRole_DenialDoesNotDiscloseRoleExistence keeps the
// detailed denial message safe to return: it must read identically whether or
// not the role exists, which a lookup added ahead of the gate would break.
func TestSTSRequest_AssumeRole_DenialDoesNotDiscloseRoleExistence(t *testing.T) {
	body := "Action=AssumeRole&RoleArn=" + stsTestRoleARN + "&RoleSessionName=s1"

	existing := assertSTSAccessDenied(t, stsPolicyRequest(t, stsIdentityPolicy(), nil, body))
	missing := assertSTSAccessDenied(t,
		stsPolicyRequest(t, stsIdentityPolicy().withRoles(nil), nil, body))

	assert.Equal(t, stripRequestID(existing), stripRequestID(missing))
	assert.Contains(t, existing, "is not authorized to perform: sts:AssumeRole on resource: "+stsTestRoleARN)
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
// requires no permission and cannot be denied by policy.
func TestSTSRequest_GetCallerIdentity_NotGated(t *testing.T) {
	svc := stsIdentityPolicy(stsStatement("Deny", "sts:*", "*"))
	resp := stsPolicyRequest(t, svc, &flexMockSTSService{}, "Action=GetCallerIdentity")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	b, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(b), "GetCallerIdentityResult")
}
