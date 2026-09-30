package gateway

import (
	"errors"
	"net/http"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/arn"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const eksNodeRoleARN = "arn:aws:iam::" + authzAccountID + ":role/eks/node-role"

// roleStoreIAMService evaluates policy from docs and resolves role names to the
// ARNs in roles, the way the IAM store does.
type roleStoreIAMService struct {
	policyMockIAMService

	roles map[string]string
}

func (s *roleStoreIAMService) CanonicalResourceARN(_ string, _ arn.IAMResourceType, name string) (string, error) {
	if roleARN, ok := s.roles[name]; ok {
		return roleARN, nil
	}
	return "", errors.New(awserrors.ErrorIAMNoSuchEntity)
}

func eksPassRoleGateway(statements ...handlers_iam.Statement) *GatewayConfig {
	docs := []handlers_iam.PolicyDocument{{Version: "2012-10-17", Statement: statements}}
	return &GatewayConfig{
		DisableLogging: true,
		Region:         authzRegion,
		IAMService: &roleStoreIAMService{
			policyMockIAMService: policyMockIAMService{
				getUserPoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) { return docs, nil },
			},
			roles: map[string]string{"node-role": eksNodeRoleARN},
		},
	}
}

func createNodegroupBody(nodeRole string) string {
	return `{"nodegroupName":"workers","nodeRole":"` + nodeRole + `"}`
}

func assertInvalidParameter(t *testing.T, err error) {
	t.Helper()
	code, ok := awserrors.ResolveErrorCode(err)
	require.True(t, ok, "unresolvable error code: %v", err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, code)
}

// eks:* alone must not let a caller launch nodes under a role it cannot pass.
func TestEKSCreateNodegroup_DeniedWithoutPassRole(t *testing.T) {
	gw := eksPassRoleGateway(statement("Allow", "eks:*", "*"))
	assertDenied(t, dispatchEKS(t, gw, http.MethodPost, "/clusters/prod/node-groups", createNodegroupBody(eksNodeRoleARN)))
}

func TestEKSCreateNodegroup_AllowedWithPassRoleOnRole(t *testing.T) {
	gw := eksPassRoleGateway(
		statement("Allow", "eks:*", "*"),
		statement("Allow", "iam:PassRole", eksNodeRoleARN),
	)
	assertPermitted(t, dispatchEKS(t, gw, http.MethodPost, "/clusters/prod/node-groups", createNodegroupBody(eksNodeRoleARN)))
}

// A grant scoped to a path the role is not under must not be satisfied by
// spelling the role's name under that path.
func TestEKSCreateNodegroup_InventedPathIsRefused(t *testing.T) {
	gw := eksPassRoleGateway(
		statement("Allow", "eks:*", "*"),
		statement("Allow", "iam:PassRole", "arn:aws:iam::"+authzAccountID+":role/sandbox/*"),
	)
	err := dispatchEKS(t, gw, http.MethodPost, "/clusters/prod/node-groups",
		createNodegroupBody("arn:aws:iam::"+authzAccountID+":role/sandbox/node-role"))
	assertInvalidParameter(t, err)
}

func TestEKSCreateNodegroup_ForeignAccountRoleIsRefused(t *testing.T) {
	gw := eksPassRoleGateway(
		statement("Allow", "eks:*", "*"),
		statement("Allow", "iam:PassRole", "*"),
	)
	err := dispatchEKS(t, gw, http.MethodPost, "/clusters/prod/node-groups",
		createNodegroupBody("arn:aws:iam::999999999999:role/eks/node-role"))
	assertInvalidParameter(t, err)
}

// An unknown role is reported only to a caller allowed to pass it, so the
// reply cannot be used to enumerate roles.
func TestEKSCreateNodegroup_UnknownRole(t *testing.T) {
	body := createNodegroupBody("arn:aws:iam::" + authzAccountID + ":role/ghost")

	denied := eksPassRoleGateway(statement("Allow", "eks:*", "*"))
	assertDenied(t, dispatchEKS(t, denied, http.MethodPost, "/clusters/prod/node-groups", body))

	allowed := eksPassRoleGateway(
		statement("Allow", "eks:*", "*"),
		statement("Allow", "iam:PassRole", "*"),
	)
	assertInvalidParameter(t, dispatchEKS(t, allowed, http.MethodPost, "/clusters/prod/node-groups", body))
}

func TestEKSCreateNodegroup_NoRoleSkipsPassRole(t *testing.T) {
	gw := eksPassRoleGateway(statement("Allow", "eks:*", "*"))
	assertPermitted(t, dispatchEKS(t, gw, http.MethodPost, "/clusters/prod/node-groups", `{"nodegroupName":"workers"}`))
}

func TestEKSCreateCluster_PassRole(t *testing.T) {
	body := `{"name":"prod","roleArn":"` + eksNodeRoleARN + `"}`

	denied := eksPassRoleGateway(statement("Allow", "eks:*", "*"))
	assertDenied(t, dispatchEKS(t, denied, http.MethodPost, "/clusters", body))

	allowed := eksPassRoleGateway(
		statement("Allow", "eks:*", "*"),
		statement("Allow", "iam:PassRole", eksNodeRoleARN),
	)
	assertPermitted(t, dispatchEKS(t, allowed, http.MethodPost, "/clusters", body))
}
