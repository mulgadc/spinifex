package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	"github.com/mulgadc/spinifex/spinifex/foundation/release/featurelevel"
)

// endpointProxyStatusWait bounds the heartbeat's ask of its own vpcd.
const endpointProxyStatusWait = 2 * time.Second

// localEndpointProxyStatus asks this node's vpcd for its endpoint proxy status.
func (d *Daemon) localEndpointProxyStatus(ctx context.Context) (projection.EndpointProxyStatus, error) {
	var st projection.EndpointProxyStatus
	if d.natsConn == nil {
		return st, errors.New("no NATS connection")
	}
	body, err := json.Marshal(projection.EndpointProxyStatusRequest{Node: d.node})
	if err != nil {
		return st, err
	}
	msg, err := d.natsConn.RequestWithContext(ctx, projection.EndpointProxyStatusSubject(d.node), body)
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(msg.Data, &st); err != nil {
		return st, fmt.Errorf("decode endpoint proxy status: %w", err)
	}
	switch {
	case st.Error != "":
		return st, fmt.Errorf("vpcd refused: %s", st.Error)
	case st.Node != d.node:
		return st, fmt.Errorf("endpoint proxy status for %q answered by %q", d.node, st.Node)
	}
	return st, nil
}

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
	st, err := d.localEndpointProxyStatus(ctx)
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
