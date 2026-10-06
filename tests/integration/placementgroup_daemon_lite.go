//go:build integration

package integration

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ec2placementgroup "github.com/mulgadc/spinifex/spinifex/domains/ec2/placementgroup"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// StartPlacementGroupDaemonLite subscribes a real
// ec2placementgroup.PlacementGroupServiceImpl — the same production
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
func StartPlacementGroupDaemonLite(t *testing.T, gw *Gateway) *ec2placementgroup.PlacementGroupServiceImpl {
	t.Helper()

	cfg := &config.Config{AZ: testAZ}
	svc, err := ec2placementgroup.NewPlacementGroupServiceImplWithNATS(t.Context(), cfg, gw.NATSConn)
	require.NoError(t, err, "construct placement group service")

	nc := gw.NATSConn
	subscribeServiceMethods(t, nc, "ec2", svc)
	sub(t, nc, "ec2.RemoveInstanceFromPlacementGroup", func(m *nats.Msg) { dispatch(m, svc.RemoveInstance) })

	return svc
}
