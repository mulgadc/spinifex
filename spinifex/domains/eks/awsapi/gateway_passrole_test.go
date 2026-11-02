package awsapi_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/auth"
	"github.com/mulgadc/spinifex/internal/testkit/gwtest"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/passrole"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	eksNodeRoleARN   = "arn:aws:iam::" + gwtest.AccountID + ":role/eks/node-role"
	foreignAccountID = "999999999999"
)

// roleStore resolves role names to the ARNs in roles, keyed by account then
// name, the way the IAM store does.
type roleStore struct {
	roles        map[string]map[string]string
	canonicalErr error
}

func (s *roleStore) ResolveRole(accountID, roleARN string) (passrole.Role, error) {
	roleAccount, name, err := auth.ParseRoleARN(roleARN)
	if err != nil {
		return passrole.Role{}, passrole.ErrMalformed
	}
	if roleAccount != accountID {
		return passrole.Role{}, passrole.ErrForeignAccount
	}
	canonical, err := s.canonical(roleAccount, name)
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

func (s *roleStore) canonical(account, name string) (string, error) {
	if s.canonicalErr != nil {
		return "", s.canonicalErr
	}
	if roleARN, ok := s.roles[account][name]; ok {
		return roleARN, nil
	}
	return "", errors.New(awserrors.ErrorIAMNoSuchEntity)
}

// The foreign account holds a role of the same name, so only the account
// check stops its ARN from resolving.
func eksPassRole(statements ...iamdomain.Statement) *eksGateway {
	return eksPassRoleFaulting(nil, statements...)
}

// eksPassRoleFaulting is eksPassRole with every role lookup failing with
// canonicalErr when it is non-nil.
func eksPassRoleFaulting(canonicalErr error, statements ...iamdomain.Statement) *eksGateway {
	store := &roleStore{canonicalErr: canonicalErr, roles: map[string]map[string]string{
		gwtest.AccountID: {"node-role": eksNodeRoleARN},
		foreignAccountID: {"node-role": "arn:aws:iam::" + foreignAccountID + ":role/eks/node-role"},
	}}
	g := scopedEKS(statements...)
	g.roles = store
	return g
}

func createNodegroupBody(nodeRole string) string {
	return `{"nodegroupName":"workers","nodeRole":"` + nodeRole + `"}`
}

func createAddonBody(roleARN string) string {
	return `{"addonName":"aws-load-balancer-controller","serviceAccountRoleArn":"` + roleARN + `"}`
}

func updateAddonBody(roleARN string) string {
	return `{"serviceAccountRoleArn":"` + roleARN + `"}`
}

// The AWS capture of 2026-10-30 refuses a foreign or non-role ARN on CreateCluster
// and CreateNodegroup with AccessDeniedException before PassRole is evaluated.
func assertCrossAccountPassRole(t *testing.T, err error) {
	t.Helper()
	code, ok := awserrors.ResolveErrorCode(err)
	require.True(t, ok, "unresolvable error code: %v", err)
	assert.Equal(t, awserrors.ErrorAccessDeniedException, code)
	assert.Contains(t, err.Error(), "Cross-account pass role is not allowed.")
}

func assertInvalidParameter(t *testing.T, err error) {
	t.Helper()
	code, ok := awserrors.ResolveErrorCode(err)
	require.True(t, ok, "unresolvable error code: %v", err)
	assert.Equal(t, awserrors.ErrorEKSInvalidParameter, code)
}

func TestEKSCreateAddon_NoRoleSkipsPassRole(t *testing.T) {
	gw := eksPassRole(gwtest.Statement("Allow", "eks:*", "*"))
	gwtest.AssertPermitted(t, dispatchEKS(t, gw, http.MethodPost, "/clusters/prod/addons", createAddonBody("")))
}

func TestEKSCreateAddon_PassRole(t *testing.T) {
	body := createAddonBody(eksNodeRoleARN)

	denied := eksPassRole(gwtest.Statement("Allow", "eks:*", "*"))
	gwtest.AssertDenied(t, dispatchEKS(t, denied, http.MethodPost, "/clusters/prod/addons", body))

	allowed := eksPassRole(
		gwtest.Statement("Allow", "eks:*", "*"),
		gwtest.Statement("Allow", "iam:PassRole", eksNodeRoleARN),
	)
	gwtest.AssertPermitted(t, dispatchEKS(t, allowed, http.MethodPost, "/clusters/prod/addons", body))
}

func TestEKSCreateCluster_PassRole(t *testing.T) {
	body := `{"name":"prod","roleArn":"` + eksNodeRoleARN + `"}`

	denied := eksPassRole(gwtest.Statement("Allow", "eks:*", "*"))
	gwtest.AssertDenied(t, dispatchEKS(t, denied, http.MethodPost, "/clusters", body))

	allowed := eksPassRole(
		gwtest.Statement("Allow", "eks:*", "*"),
		gwtest.Statement("Allow", "iam:PassRole", eksNodeRoleARN),
	)
	gwtest.AssertPermitted(t, dispatchEKS(t, allowed, http.MethodPost, "/clusters", body))
}

func TestEKSCreateNodegroup_AllowedWithPassRoleOnRole(t *testing.T) {
	gw := eksPassRole(
		gwtest.Statement("Allow", "eks:*", "*"),
		gwtest.Statement("Allow", "iam:PassRole", eksNodeRoleARN),
	)
	gwtest.AssertPermitted(t, dispatchEKS(t, gw, http.MethodPost, "/clusters/prod/node-groups", createNodegroupBody(eksNodeRoleARN)))
}

// eks:* alone must not let a caller launch nodes under a role it cannot pass.
func TestEKSCreateNodegroup_DeniedWithoutPassRole(t *testing.T) {
	gw := eksPassRole(gwtest.Statement("Allow", "eks:*", "*"))
	gwtest.AssertDenied(t, dispatchEKS(t, gw, http.MethodPost, "/clusters/prod/node-groups", createNodegroupBody(eksNodeRoleARN)))
}

func TestEKSCreateNodegroup_ForeignAccountRoleIsRefused(t *testing.T) {
	gw := eksPassRole(
		gwtest.Statement("Allow", "eks:*", "*"),
		gwtest.Statement("Allow", "iam:PassRole", "*"),
	)
	err := dispatchEKS(t, gw, http.MethodPost, "/clusters/prod/node-groups",
		createNodegroupBody("arn:aws:iam::"+foreignAccountID+":role/eks/node-role"))
	assertCrossAccountPassRole(t, err)
}

// A grant scoped to a path the role is not under must not be satisfied by
// spelling the role's name under that path.
func TestEKSCreateNodegroup_InventedPathIsRefused(t *testing.T) {
	gw := eksPassRole(
		gwtest.Statement("Allow", "eks:*", "*"),
		gwtest.Statement("Allow", "iam:PassRole", "arn:aws:iam::"+gwtest.AccountID+":role/sandbox/*"),
	)
	err := dispatchEKS(t, gw, http.MethodPost, "/clusters/prod/node-groups",
		createNodegroupBody("arn:aws:iam::"+gwtest.AccountID+":role/sandbox/node-role"))
	assertInvalidParameter(t, err)
}

// A store fault is not evidence the role is absent, so it must not surface as
// a caller error or fall through to dispatch.
func TestEKSCreateNodegroup_LookupFaultPassesThrough(t *testing.T) {
	storeErr := errors.New("get role: nats: timeout")
	gw := eksPassRoleFaulting(storeErr,
		gwtest.Statement("Allow", "eks:*", "*"),
		gwtest.Statement("Allow", "iam:PassRole", "*"),
	)
	err := dispatchEKS(t, gw, http.MethodPost, "/clusters/prod/node-groups", createNodegroupBody(eksNodeRoleARN))
	require.ErrorIs(t, err, storeErr)
}

func TestEKSCreateNodegroup_NoRoleSkipsPassRole(t *testing.T) {
	gw := eksPassRole(gwtest.Statement("Allow", "eks:*", "*"))
	gwtest.AssertPermitted(t, dispatchEKS(t, gw, http.MethodPost, "/clusters/prod/node-groups", `{"nodegroupName":"workers"}`))
}

func TestEKSCreateNodegroup_NonRoleARNIsRefused(t *testing.T) {
	gw := eksPassRole(
		gwtest.Statement("Allow", "eks:*", "*"),
		gwtest.Statement("Allow", "iam:PassRole", "*"),
	)
	err := dispatchEKS(t, gw, http.MethodPost, "/clusters/prod/node-groups",
		createNodegroupBody("arn:aws:iam::"+gwtest.AccountID+":user/node-role"))
	assertCrossAccountPassRole(t, err)
}

// Current behaviour: a store fault on the role lookup is returned before the
// PassRole check, so a caller without iam:PassRole sees the fault, not a denial.
func TestEKSCreateNodegroup_OutageIsReportedToACallerWithoutPassRole(t *testing.T) {
	storeErr := errors.New("get role: nats: timeout")
	gw := eksPassRoleFaulting(storeErr, gwtest.Statement("Allow", "eks:*", "*"))
	err := dispatchEKS(t, gw, http.MethodPost, "/clusters/prod/node-groups", createNodegroupBody(eksNodeRoleARN))
	require.ErrorIs(t, err, storeErr)
}

// An unknown role is reported only to a caller allowed to pass it, so the
// reply cannot be used to enumerate roles.
func TestEKSCreateNodegroup_UnknownRole(t *testing.T) {
	body := createNodegroupBody("arn:aws:iam::" + gwtest.AccountID + ":role/ghost")

	denied := eksPassRole(gwtest.Statement("Allow", "eks:*", "*"))
	gwtest.AssertDenied(t, dispatchEKS(t, denied, http.MethodPost, "/clusters/prod/node-groups", body))

	allowed := eksPassRole(
		gwtest.Statement("Allow", "eks:*", "*"),
		gwtest.Statement("Allow", "iam:PassRole", "*"),
	)
	assertInvalidParameter(t, dispatchEKS(t, allowed, http.MethodPost, "/clusters/prod/node-groups", body))
}

func TestEKSUpdateAddon_NoRoleSkipsPassRole(t *testing.T) {
	gw := eksPassRole(gwtest.Statement("Allow", "eks:*", "*"))
	gwtest.AssertPermitted(t, dispatchEKS(t, gw, http.MethodPost,
		"/clusters/prod/addons/aws-load-balancer-controller/update", updateAddonBody("")))
}

func TestEKSUpdateAddon_PassRole(t *testing.T) {
	body := updateAddonBody(eksNodeRoleARN)
	const path = "/clusters/prod/addons/aws-load-balancer-controller/update"

	denied := eksPassRole(gwtest.Statement("Allow", "eks:*", "*"))
	gwtest.AssertDenied(t, dispatchEKS(t, denied, http.MethodPost, path, body))

	allowed := eksPassRole(
		gwtest.Statement("Allow", "eks:*", "*"),
		gwtest.Statement("Allow", "iam:PassRole", eksNodeRoleARN),
	)
	gwtest.AssertPermitted(t, dispatchEKS(t, allowed, http.MethodPost, path, body))
}
