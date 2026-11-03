package awsgw

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/accountcleanup"
	"github.com/mulgadc/spinifex/spinifex/operator/accountlifecycle"
	operatoradmin "github.com/mulgadc/spinifex/spinifex/operator/admin"

	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	stsawsapi "github.com/mulgadc/spinifex/spinifex/domains/sts/awsapi"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/accountadmission"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/credverify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A hook left unset in production fails requests closed or drops a route, so
// the composed config must carry every one of them.
func TestComposeGateway_SetsEveryProductionHook(t *testing.T) {
	gw, err := composeGateway(gatewayInputs{
		Credentials:  credverify.New(nil, nil),
		Admission:    accountadmission.New(nil),
		PublicRoutes: PublicRoutes(nil, nil),
		PreAuth:      []dispatch.PreAuth{stsawsapi.NewPreAuth(stsawsapi.Deps{})},
	}, awsapi.NewRegistration(awsapi.Deps{}))
	require.NoError(t, err)

	assert.NotNil(t, gw.Services, "Services")
	assert.NotEmpty(t, gw.PreAuth, "PreAuth")
	assert.NotNil(t, gw.PublicRoutes, "PublicRoutes")
	assert.NotEmpty(t, gw.Admin.Service, "Admin.Service")
	assert.NotNil(t, gw.Admin.WithRequestID, "Admin.WithRequestID")
	assert.NotNil(t, gw.Credentials, "Credentials")
	assert.NotNil(t, gw.Policy, "Policy")
	assert.NotNil(t, gw.Admission, "Admission")
	assert.NotNil(t, gw.Readiness, "Readiness")
}

// Account termination probes the gateway node set before sealing, so awsgw
// must hand the operator surface both node sets from the cluster config.
func TestWithClusterNodes_SetsDaemonAndGatewaySets(t *testing.T) {
	cfg := &config.ClusterConfig{Node: "n2", Nodes: map[string]config.Config{
		"n1": {Services: []string{"nats", "awsgw", "daemon"}},
		"n2": {Services: []string{"daemon"}},
		"n3": {},
	}}

	got := withClusterNodes(operatoradmin.Deps{}, cfg)

	assert.Equal(t, "n2", got.NodeID, "the deletion job holder is this node")
	assert.Equal(t, 3, got.ExpectedNodes)
	assert.Equal(t, []string{"n1", "n2", "n3"}, got.NodeIDs)
	// n3 has no service list, which means every service, awsgw included.
	assert.Equal(t, []string{"n1", "n3"}, got.GatewayNodeIDs)
}

// Deletion runs the quiesce and the fenced removal, which only the concrete IAM
// service carries. awsgw must bind it, or every DeleteAccount fails.
func TestOperatorDeps_BindsTheConcreteIAMServiceAndNodes(t *testing.T) {
	cfg := &config.ClusterConfig{Node: "n1", Nodes: map[string]config.Config{"n1": {}}}
	iamService := &iamdomain.IAMServiceImpl{}

	got := operatorDeps(iamService, cfg, operatoradmin.Deps{})

	assert.Same(t, iamService, got.IAM, "IAM")
	assert.Same(t, iamService, got.Termination, "Termination")
	assert.Equal(t, []string{"n1"}, got.GatewayNodeIDs, "GatewayNodeIDs")
}

// The deletion resumer is what finishes an account deletion whose gateway died,
// with no request to prompt it. A composition that stops starting it would leave
// such jobs stuck until someone retried by hand.
func TestAwsgwStartsTheDeletionResumer(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "awsgw.go", nil, 0)
	require.NoError(t, err)

	var started bool
	ast.Inspect(file, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "StartDeletionResumer" {
			started = true
		}
		return true
	})
	assert.True(t, started, "awsgw.go must call Deps.StartDeletionResumer")
}

type fixedCleaner struct{}

func (fixedCleaner) CleanupPlatform(context.Context, *accountcleanup.Request) (*accountcleanup.Result, error) {
	return &accountcleanup.Result{}, nil
}

// Every DeleteAccount cleans through the in-process owners awsgw
// binds. A cleanup that cannot be built must leave the field nil, which refuses
// the request, rather than bind something half-working.
func TestWithPlatformCleaner_BindsTheCleanupOrLeavesItNil(t *testing.T) {
	bound := withPlatformCleaner(operatoradmin.Deps{}, func() (accountlifecycle.PlatformCleaner, error) { return fixedCleaner{}, nil })
	assert.NotNil(t, bound.PlatformCleaner)

	unbound := withPlatformCleaner(operatoradmin.Deps{}, func() (accountlifecycle.PlatformCleaner, error) {
		return nil, errors.New("owner store unavailable")
	})
	assert.Nil(t, unbound.PlatformCleaner)
}

// The cleanup is bound where the operator deps are composed, or every delete is
// refused on every gateway.
func TestAwsgwBindsThePlatformCleanup(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "awsgw.go", nil, 0)
	require.NoError(t, err)

	var bound bool
	ast.Inspect(file, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "withPlatformCleaner" {
				bound = true
			}
		}
		return true
	})
	assert.True(t, bound, "awsgw.go must call withPlatformCleaner")
}

// The ordinary delete may remove a live launch template only when EKS says no
// nodegroup names it, so the cleanup must be built with the EKS-backed reader.
func TestPlatformCleanerIsBuiltWithTheEKSTemplateConsumerReader(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "platform_cleanup.go", nil, 0)
	require.NoError(t, err)

	var wired bool
	ast.Inspect(file, func(n ast.Node) bool {
		if kv, ok := n.(*ast.KeyValueExpr); ok {
			if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Consumers" {
				if call, ok := kv.Value.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "NewTemplateConsumerReader" {
						wired = true
					}
				}
			}
		}
		return true
	})
	assert.True(t, wired, "platformCleaner must set ReferenceReaders.Consumers to accountlifecycle.NewTemplateConsumerReader")
}
