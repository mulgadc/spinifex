package daemon

import (
	"bytes"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/masterkey"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Without a base domain no service can name its records, so none of them may
// claim prune authority over the zone.
func TestDNSDesiredSet_ServicesWithoutBaseDomain(t *testing.T) {
	d := &Daemon{
		elbv2Service: &handlers_elbv2.ELBv2ServiceImpl{},
		eksService:   &handlers_eks.EKSServiceImpl{},
		rdsService:   &handlers_rds.Service{},
	}

	ds := d.dnsDesiredSet()
	assert.False(t, ds.Prunable.ELB)
	assert.False(t, ds.Prunable.EKS)
	assert.False(t, ds.Prunable.RDS)
	assert.Empty(t, ds.Changes)
}

func TestDNSDesiredSet_ServicesEnumerateCompletely(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	cfg := &config.Config{Region: "ap-southeast-2"}
	cfg.Northstar.DefaultDomain = "example.internal"

	elb, err := handlers_elbv2.NewELBv2ServiceImplWithNATS(cfg, nc, bytes.Repeat([]byte{0x42}, masterkey.MasterKeySize))
	require.NoError(t, err)
	t.Cleanup(elb.Close)
	eks, err := handlers_eks.NewEKSServiceImpl(handlers_eks.EKSServiceDeps{NATSConn: nc, Config: cfg})
	require.NoError(t, err)
	t.Cleanup(eks.Shutdown)
	rds := handlers_rds.NewService(nc, cfg.Region).WithDeps(handlers_rds.Deps{BaseDomain: "example.internal"})

	d := &Daemon{elbv2Service: elb, eksService: eks, rdsService: rds}
	ds := d.dnsDesiredSet()
	assert.True(t, ds.Prunable.ELB, "an empty load balancer store is a complete view")
	assert.True(t, ds.Prunable.EKS)
	assert.True(t, ds.Prunable.RDS)
	assert.False(t, ds.Prunable.EC2, "no instance store was configured")
	assert.Empty(t, ds.Changes)
}
