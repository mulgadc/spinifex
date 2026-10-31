package gateway

import (
	"errors"
	"github.com/stretchr/testify/require"
	"net/http"
	"testing"
)

func TestEKSCreateAddon_NoRoleSkipsPassRole(t *testing.T) {
	gw := eksPassRoleGateway(statement("Allow", "eks:*", "*"))
	assertPermitted(t, dispatchEKS(t, gw, http.MethodPost, "/clusters/prod/addons", createAddonBody("")))
}

func TestEKSCreateAddon_PassRole(t *testing.T) {
	body := createAddonBody(eksNodeRoleARN)

	denied := eksPassRoleGateway(statement("Allow", "eks:*", "*"))
	assertDenied(t, dispatchEKS(t, denied, http.MethodPost, "/clusters/prod/addons", body))

	allowed := eksPassRoleGateway(
		statement("Allow", "eks:*", "*"),
		statement("Allow", "iam:PassRole", eksNodeRoleARN),
	)
	assertPermitted(t, dispatchEKS(t, allowed, http.MethodPost, "/clusters/prod/addons", body))
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

func TestEKSCreateNodegroup_AllowedWithPassRoleOnRole(t *testing.T) {
	gw := eksPassRoleGateway(
		statement("Allow", "eks:*", "*"),
		statement("Allow", "iam:PassRole", eksNodeRoleARN),
	)
	assertPermitted(t, dispatchEKS(t, gw, http.MethodPost, "/clusters/prod/node-groups", createNodegroupBody(eksNodeRoleARN)))
}

// eks:* alone must not let a caller launch nodes under a role it cannot pass.
func TestEKSCreateNodegroup_DeniedWithoutPassRole(t *testing.T) {
	gw := eksPassRoleGateway(statement("Allow", "eks:*", "*"))
	assertDenied(t, dispatchEKS(t, gw, http.MethodPost, "/clusters/prod/node-groups", createNodegroupBody(eksNodeRoleARN)))
}

func TestEKSCreateNodegroup_ForeignAccountRoleIsRefused(t *testing.T) {
	gw := eksPassRoleGateway(
		statement("Allow", "eks:*", "*"),
		statement("Allow", "iam:PassRole", "*"),
	)
	err := dispatchEKS(t, gw, http.MethodPost, "/clusters/prod/node-groups",
		createNodegroupBody("arn:aws:iam::"+foreignAccountID+":role/eks/node-role"))
	assertCrossAccountPassRole(t, err)
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

// A store fault is not evidence the role is absent, so it must not surface as
// a caller error or fall through to dispatch.
func TestEKSCreateNodegroup_LookupFaultPassesThrough(t *testing.T) {
	storeErr := errors.New("get role: nats: timeout")
	iamService := eksPassRoleService(
		statement("Allow", "eks:*", "*"),
		statement("Allow", "iam:PassRole", "*"),
	)
	iamService.canonicalErr = storeErr
	err := dispatchEKS(t, eksGatewayWithIAM(iamService), http.MethodPost, "/clusters/prod/node-groups", createNodegroupBody(eksNodeRoleARN))
	require.ErrorIs(t, err, storeErr)
}

func TestEKSCreateNodegroup_NoRoleSkipsPassRole(t *testing.T) {
	gw := eksPassRoleGateway(statement("Allow", "eks:*", "*"))
	assertPermitted(t, dispatchEKS(t, gw, http.MethodPost, "/clusters/prod/node-groups", `{"nodegroupName":"workers"}`))
}

func TestEKSCreateNodegroup_NonRoleARNIsRefused(t *testing.T) {
	gw := eksPassRoleGateway(
		statement("Allow", "eks:*", "*"),
		statement("Allow", "iam:PassRole", "*"),
	)
	err := dispatchEKS(t, gw, http.MethodPost, "/clusters/prod/node-groups",
		createNodegroupBody("arn:aws:iam::"+authzAccountID+":user/node-role"))
	assertCrossAccountPassRole(t, err)
}

// Current behaviour: a store fault on the role lookup is returned before the
// PassRole check, so a caller without iam:PassRole sees the fault, not a denial.
func TestEKSCreateNodegroup_OutageIsReportedToACallerWithoutPassRole(t *testing.T) {
	storeErr := errors.New("get role: nats: timeout")
	iamService := eksPassRoleService(statement("Allow", "eks:*", "*"))
	iamService.canonicalErr = storeErr
	err := dispatchEKS(t, eksGatewayWithIAM(iamService), http.MethodPost, "/clusters/prod/node-groups", createNodegroupBody(eksNodeRoleARN))
	require.ErrorIs(t, err, storeErr)
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

func TestEKSUpdateAddon_NoRoleSkipsPassRole(t *testing.T) {
	gw := eksPassRoleGateway(statement("Allow", "eks:*", "*"))
	assertPermitted(t, dispatchEKS(t, gw, http.MethodPost,
		"/clusters/prod/addons/aws-load-balancer-controller/update", updateAddonBody("")))
}

func TestEKSUpdateAddon_PassRole(t *testing.T) {
	body := updateAddonBody(eksNodeRoleARN)
	const path = "/clusters/prod/addons/aws-load-balancer-controller/update"

	denied := eksPassRoleGateway(statement("Allow", "eks:*", "*"))
	assertDenied(t, dispatchEKS(t, denied, http.MethodPost, path, body))

	allowed := eksPassRoleGateway(
		statement("Allow", "eks:*", "*"),
		statement("Allow", "iam:PassRole", eksNodeRoleARN),
	)
	assertPermitted(t, dispatchEKS(t, allowed, http.MethodPost, path, body))
}
