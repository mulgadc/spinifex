package awsgw

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	operatoradmin "github.com/mulgadc/spinifex/spinifex/operator/admin"

	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
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
	cfg := &config.ClusterConfig{Nodes: map[string]config.Config{
		"n1": {Services: []string{"nats", "awsgw", "daemon"}},
		"n2": {Services: []string{"daemon"}},
		"n3": {},
	}}

	got := withClusterNodes(operatoradmin.Deps{}, cfg)

	assert.Equal(t, 3, got.ExpectedNodes)
	assert.Equal(t, []string{"n1", "n2", "n3"}, got.NodeIDs)
	// n3 has no service list, which means every service, awsgw included.
	assert.Equal(t, []string{"n1", "n3"}, got.GatewayNodeIDs)
}
