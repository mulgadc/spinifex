//go:build integration

package integration

import (
	"path/filepath"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	acmdomain "github.com/mulgadc/spinifex/spinifex/domains/acm"
	ecsdomain "github.com/mulgadc/spinifex/spinifex/domains/ecs"
	ekscluster "github.com/mulgadc/spinifex/spinifex/domains/eks/cluster"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	rdsinstance "github.com/mulgadc/spinifex/spinifex/domains/rds/instance"
	route53domain "github.com/mulgadc/spinifex/spinifex/domains/route53"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// StartServiceDaemonLite subscribes the real ACM, ECS, EKS, ELBv2, RDS and Route 53
// services to their "<service>.<Method>" subjects. ECS and RDS run without
// their provisioning dependencies, so checks that need them do not run.
func StartServiceDaemonLite(t *testing.T, gw *Gateway) {
	t.Helper()
	nc := gw.NATSConn
	cfg := &config.Config{AZ: testAZ, Region: testRegion}
	masterKey, err := iamdomain.GenerateMasterKey()
	require.NoError(t, err)

	acm, err := acmdomain.NewACMServiceImplWithNATS(t.Context(), cfg, nc, masterKey)
	require.NoError(t, err, "construct ACM service")
	// Without a tenant CA no mode can issue, so every RequestCertificate would
	// be refused. The generated requests name example.com domains.
	caDir := t.TempDir()
	acm.TenantCA, err = acmdomain.LoadOrCreateTenantCA(filepath.Join(caDir, "ca.pem"), filepath.Join(caDir, "ca.key"), []string{"example.com"})
	require.NoError(t, err, "create ACM tenant CA")
	subscribeServiceMethods(t, nc, "acm", acm)

	elbv2, err := elbv2domain.NewELBv2ServiceImplWithNATS(cfg, nc, masterKey)
	require.NoError(t, err, "construct ELBv2 service")
	t.Cleanup(elbv2.Close)
	subscribeServiceMethods(t, nc, "elbv2", elbv2)

	eks, err := ekscluster.NewEKSServiceImpl(ekscluster.EKSServiceDeps{
		Config: cfg, NATSConn: nc, MasterKey: masterKey, Region: testRegion, HolderID: "integration-test-node",
	})
	require.NoError(t, err, "construct EKS service")
	t.Cleanup(eks.Shutdown)
	subscribeServiceMethods(t, nc, "eks", eks)

	subscribeServiceMethods(t, nc, "ecs", ecsdomain.NewService(nc, testRegion, ""))
	subscribeServiceMethods(t, nc, "rds", rdsinstance.NewService(nc, testRegion))

	js, err := jetstream.New(nc)
	require.NoError(t, err, "open JetStream for Route 53")
	route53 := route53domain.NewService(route53domain.NewStore(js))
	route53.Nameservers = func() []string { return []string{"ns1.spx3.net"} }
	subscribeServiceMethods(t, nc, "route53", route53)
}
