//go:build integration

package integration

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/config"
	handlers_ec2_placementgroup "github.com/mulgadc/spinifex/spinifex/handlers/ec2/placementgroup"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// StartPlacementGroupDaemonLite subscribes a real
// handlers_ec2_placementgroup.PlacementGroupServiceImpl — the same production
// code a live daemon runs — to every ec2.*PlacementGroup*/ec2.Reserve*/
// ec2.Finalize*/ec2.Release* subject the gateway's NATSPlacementGroupService
// client calls (gateway/ec2/placementgroup/*.go and RunInstances.go's spread/
// cluster routing in gateway/ec2/instance/placement.go). Unlike a StubSubject
// canned reply, this exercises the real KV-backed CAS reservation logic, so a
// test that checks placement-group strategy routing actually proves the
// gateway drove a genuine reserve/finalize round trip rather than merely
// reaching some daemon-shaped subject.
//
// Kept separate from StartDaemonLite (matching StartLaunchTemplateDaemonLite's
// precedent): only a test that actually exercises placement groups should pay
// for wiring this service.
func StartPlacementGroupDaemonLite(t *testing.T, gw *Gateway) *handlers_ec2_placementgroup.PlacementGroupServiceImpl {
	t.Helper()

	cfg := &config.Config{AZ: testAZ}
	svc, err := handlers_ec2_placementgroup.NewPlacementGroupServiceImplWithNATS(cfg, gw.NATSConn)
	require.NoError(t, err, "construct placement group service")

	nc := gw.NATSConn
	sub(t, nc, "ec2.CreatePlacementGroup", func(m *nats.Msg) { dispatch(m, svc.CreatePlacementGroup) })
	sub(t, nc, "ec2.DeletePlacementGroup", func(m *nats.Msg) { dispatch(m, svc.DeletePlacementGroup) })
	sub(t, nc, "ec2.DescribePlacementGroups", func(m *nats.Msg) { dispatch(m, svc.DescribePlacementGroups) })
	sub(t, nc, "ec2.ReserveSpreadNodes", func(m *nats.Msg) { dispatch(m, svc.ReserveSpreadNodes) })
	sub(t, nc, "ec2.FinalizeSpreadInstances", func(m *nats.Msg) { dispatch(m, svc.FinalizeSpreadInstances) })
	sub(t, nc, "ec2.ReleaseSpreadNodes", func(m *nats.Msg) { dispatch(m, svc.ReleaseSpreadNodes) })
	sub(t, nc, "ec2.RemoveInstanceFromPlacementGroup", func(m *nats.Msg) { dispatch(m, svc.RemoveInstance) })
	sub(t, nc, "ec2.ReserveClusterNode", func(m *nats.Msg) { dispatch(m, svc.ReserveClusterNode) })
	sub(t, nc, "ec2.FinalizeClusterInstances", func(m *nats.Msg) { dispatch(m, svc.FinalizeClusterInstances) })

	return svc
}
