//go:build integration

package integration

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/config"
	handlers_ec2_launchtemplate "github.com/mulgadc/spinifex/spinifex/handlers/ec2/launchtemplate"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// StartLaunchTemplateDaemonLite subscribes a real
// handlers_ec2_launchtemplate.LaunchTemplateServiceImpl — the same production
// code a live daemon runs (daemon/daemon_handlers_launchtemplate.go) — to
// every ec2.*LaunchTemplate* subject the gateway's NATSLaunchTemplateService
// client calls (gateway/ec2/launchtemplate/*.go, RunInstances.go's
// expandLaunchTemplate). Unlike StubSubject's static canned JSON, this
// exercises the real KV-backed CRUD and version-resolution logic, matching
// how RunInstances resolves a referenced template into effective ImageId/
// InstanceType before the per-node launch dispatch.
//
// Kept separate from StartDaemonLite (rather than folded into it) following
// StartVolumeDaemonLite's precedent: only a test that actually exercises
// launch templates should pay for wiring this service.
func StartLaunchTemplateDaemonLite(t *testing.T, gw *Gateway) *handlers_ec2_launchtemplate.LaunchTemplateServiceImpl {
	t.Helper()

	cfg := &config.Config{AZ: testAZ}
	svc, err := handlers_ec2_launchtemplate.NewLaunchTemplateServiceImplWithNATS(t.Context(), cfg, gw.NATSConn)
	require.NoError(t, err, "construct launch template service")

	nc := gw.NATSConn
	sub(t, nc, "ec2.CreateLaunchTemplate", func(m *nats.Msg) { dispatch(m, svc.CreateLaunchTemplate) })
	sub(t, nc, "ec2.CreateLaunchTemplateVersion", func(m *nats.Msg) { dispatch(m, svc.CreateLaunchTemplateVersion) })
	sub(t, nc, "ec2.DeleteLaunchTemplate", func(m *nats.Msg) { dispatch(m, svc.DeleteLaunchTemplate) })
	sub(t, nc, "ec2.DeleteLaunchTemplateVersions", func(m *nats.Msg) { dispatch(m, svc.DeleteLaunchTemplateVersions) })
	sub(t, nc, "ec2.ModifyLaunchTemplate", func(m *nats.Msg) { dispatch(m, svc.ModifyLaunchTemplate) })
	sub(t, nc, "ec2.DescribeLaunchTemplates", func(m *nats.Msg) { dispatch(m, svc.DescribeLaunchTemplates) })
	sub(t, nc, "ec2.DescribeLaunchTemplateVersions", func(m *nats.Msg) { dispatch(m, svc.DescribeLaunchTemplateVersions) })

	return svc
}
