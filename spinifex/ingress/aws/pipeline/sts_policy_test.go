package pipeline

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"testing"
)

// TestSTSRequest_AssumeRole_AssumedRoleCallerIsGated is the case that would
// have caught the ECS agent regression: a role session whose underlying role
// grants a non-sts action and nothing else must not be able to assume anything.
func TestSTSRequest_AssumeRole_AssumedRoleCallerIsGated(t *testing.T) {
	body := "Action=AssumeRole&RoleArn=" + stsTestRoleARN + "&RoleSessionName=s1"
	params := func(iamSvc iamdomain.IAMService, stsSvc *flexMockSTSService) stsRequestParams {
		return stsRequestParams{
			accountID:         awsidentifiers.GlobalAccountID,
			identity:          "s1",
			principalType:     gwfix.PrincipalTypeAssumedRole,
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

	// A pathed role is where the two can diverge: a resolved role names its
	// stored ARN, so an unresolved one must name what the caller supplied
	// rather than any form derived from it.
	const pathed = "arn:aws:iam::000000000000:role/team/svc"
	pathedBody := "Action=AssumeRole&RoleArn=" + pathed + "&RoleSessionName=s1"

	pathedExisting := assertSTSAccessDenied(t, stsPolicyRequest(t,
		stsIdentityPolicy().withRoles(map[string]string{"svc": pathed}), nil, pathedBody))
	pathedMissing := assertSTSAccessDenied(t,
		stsPolicyRequest(t, stsIdentityPolicy().withRoles(nil), nil, pathedBody))

	assert.Equal(t, stripRequestID(pathedExisting), stripRequestID(pathedMissing))
	assert.Contains(t, pathedExisting, "on resource: "+pathed)
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

// TestSTSRequest_AssumeRole_PathIsResolvedNotTrusted pins the gate to the ARN
// IAM stored, so a role genuinely stored under a path is matched by its full
// ARN. An ARN that is not the stored one is refused by role resolution itself
// — see TestResolveRoleByARN in domains/sts.
func TestSTSRequest_AssumeRole_PathIsResolvedNotTrusted(t *testing.T) {
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

// TestSTSRequest_GetCallerIdentity_NotGated pins the AWS contract: the action
// requires no permission and cannot be denied by policy.
func TestSTSRequest_GetCallerIdentity_NotGated(t *testing.T) {
	svc := stsIdentityPolicy(stsStatement("Deny", "sts:*", "*"))
	resp := stsPolicyRequest(t, svc, &flexMockSTSService{}, "Action=GetCallerIdentity")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	b, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(b), "GetCallerIdentityResult")
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
