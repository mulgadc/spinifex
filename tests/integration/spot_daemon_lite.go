//go:build integration

package integration

import (
	"testing"

	handlers_ec2_spotinstance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/spotinstance"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// StartSpotDaemonLite subscribes a real handlers_ec2_spotinstance.SpotInstanceServiceImpl —
// the same production code a live daemon runs (daemon/daemon_handlers_spotinstance.go) — to
// the ec2.PutSpotInstanceRequests/DescribeSpotInstanceRequests/CancelSpotInstanceRequests
// subjects the gateway's NATSSpotInstanceService client calls, so RequestSpotInstances /
// DescribeSpotInstanceRequests / CancelSpotInstanceRequests exercise genuine daemon-side
// persistence instead of a StubSubject canned reply.
//
// CloseForInstance is never NATS-routed: a live daemon calls it in-process from its teardown
// cleaner (spinifex/daemon/vm_adapters.go instanceCleanerAdapter.RemoveFromSpotRequest), which
// needs a real vm.Manager and is out of scope for this tier, matching DaemonLite's own
// carve-out for instance lifecycle. The returned service exposes CloseForInstance directly so
// a test can invoke it the same way the teardown cleaner would, without standing up VM
// lifecycle machinery.
func StartSpotDaemonLite(t *testing.T, gw *Gateway) *handlers_ec2_spotinstance.SpotInstanceServiceImpl {
	t.Helper()

	svc, err := handlers_ec2_spotinstance.NewSpotInstanceServiceImplWithNATS(nil, gw.NATSConn)
	require.NoError(t, err, "construct spot instance service")

	nc := gw.NATSConn
	sub(t, nc, "ec2.PutSpotInstanceRequests", func(m *nats.Msg) { dispatch(m, svc.PutSpotInstanceRequests) })
	sub(t, nc, "ec2.DescribeSpotInstanceRequests", func(m *nats.Msg) { dispatch(m, svc.DescribeSpotInstanceRequests) })
	sub(t, nc, "ec2.CancelSpotInstanceRequests", func(m *nats.Msg) { dispatch(m, svc.CancelSpotInstanceRequests) })

	return svc
}
