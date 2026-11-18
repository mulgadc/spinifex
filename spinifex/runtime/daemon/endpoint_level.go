package daemon

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	"github.com/mulgadc/spinifex/spinifex/foundation/release/featurelevel"
)

// endpointProxyStatusWait bounds the heartbeat's ask of its own vpcd.
const endpointProxyStatusWait = 2 * time.Second

// advertisedFeatureLevel is the level this node's heartbeat reports. Gateway
// endpoints need this node to capture their traffic and its gate to serve
// endpoint proxies, so until its vpcd reports both it reports the level below.
func (d *Daemon) advertisedFeatureLevel() int {
	parent := d.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, endpointProxyStatusWait)
	defer cancel()
	st, err := projection.RequestEndpointProxyStatus(ctx, d.natsConn, d.node)
	level := featurelevel.Build
	if err != nil || !st.ProbePassed || !st.GateReady {
		level = featurelevel.GatewayEndpoints - 1
	}
	if prev := d.advertisedLevel.Swap(int64(level)); prev != int64(level) {
		slog.Info("Feature level advertised", "level", level, "build", featurelevel.Build,
			"capture_probe", st.ProbePassed, "gate_ready", st.GateReady, "err", err)
	}
	return level
}

// endpointDemuxHolding reports this node's endpoint demux to the rollback
// drain: one while it may hold any demux flow.
func (d *Daemon) endpointDemuxHolding() int {
	if d.vmMgr != nil && d.vmMgr.EndpointDemuxHeld() {
		return 1
	}
	return 0
}

// endpointOffered reports whether gateway endpoints are offered, for reads.
func endpointOffered(gate *featurelevel.Gate) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		if gate == nil {
			return false, errors.New("feature level gate not ready")
		}
		return gate.Active(ctx, featurelevel.GatewayEndpoints)
	}
}

// endpointAdmission admits gateway-endpoint writes at the feature level.
func endpointAdmission(gate *featurelevel.Gate) func(context.Context) (func(), bool, error) {
	return func(ctx context.Context) (func(), bool, error) {
		if gate == nil {
			return func() {}, false, errors.New("feature level gate not ready")
		}
		return gate.Admit(ctx, featurelevel.GatewayEndpoints)
	}
}
