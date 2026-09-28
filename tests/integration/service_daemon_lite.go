//go:build integration

package integration

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/config"
	handlers_acm "github.com/mulgadc/spinifex/spinifex/handlers/acm"
	handlers_ecs "github.com/mulgadc/spinifex/spinifex/handlers/ecs"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	"github.com/stretchr/testify/require"
)

// StartServiceDaemonLite subscribes the real ACM, ECS, EKS, ELBv2 and RDS
// services — the ones a live daemon constructs in daemon.go — to their
// "<service>.<Method>" subjects, with no provisioning backends behind them.
// Requests are validated and stored as on a live daemon; anything that would
// launch an instance or a VM fails at the NATS subject nothing answers.
func StartServiceDaemonLite(t *testing.T, gw *Gateway) {
	t.Helper()
	nc := gw.NATSConn
	cfg := &config.Config{AZ: testAZ, Region: testRegion}
	masterKey, err := handlers_iam.GenerateMasterKey()
	require.NoError(t, err)

	acm, err := handlers_acm.NewACMServiceImplWithNATS(t.Context(), cfg, nc, masterKey)
	require.NoError(t, err, "construct ACM service")
	subscribeServiceMethods(t, nc, "acm", acm)

	elbv2, err := handlers_elbv2.NewELBv2ServiceImplWithNATS(cfg, nc, masterKey)
	require.NoError(t, err, "construct ELBv2 service")
	t.Cleanup(elbv2.Close)
	subscribeServiceMethods(t, nc, "elbv2", elbv2)

	eks, err := handlers_eks.NewEKSServiceImpl(handlers_eks.EKSServiceDeps{
		Config: cfg, NATSConn: nc, MasterKey: masterKey, Region: testRegion, HolderID: "integration-test-node", ClusterSize: 1,
	})
	require.NoError(t, err, "construct EKS service")
	t.Cleanup(eks.Shutdown)
	subscribeServiceMethods(t, nc, "eks", eks)

	subscribeServiceMethods(t, nc, "ecs", handlers_ecs.NewService(nc, testRegion, ""))
	subscribeServiceMethods(t, nc, "rds", handlers_rds.NewService(nc, testRegion))
}
