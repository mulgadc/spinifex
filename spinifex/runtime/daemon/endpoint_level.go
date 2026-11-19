package daemon

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/prefixlist"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	"github.com/mulgadc/spinifex/spinifex/foundation/release/featurelevel"
)

// endpointProxyStatusWait bounds the heartbeat's ask of its own vpcd.
const endpointProxyStatusWait = 2 * time.Second

// advertisedFeatureLevel is the level this node's heartbeat reports: below 2
// until its vpcd can capture and render prefix-list peers, its gate serves
// endpoint proxies, and its prefix-list entry is the gossiped one.
func (d *Daemon) advertisedFeatureLevel() int {
	parent := d.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, endpointProxyStatusWait)
	defer cancel()
	st, err := projection.RequestEndpointProxyStatus(ctx, d.natsConn, d.node)
	agree := prefixListDerivationsAgree(d.clusterConfig, d.mgmtBridgeIP)
	level := featurelevel.Build
	if err != nil || !st.ProbePassed || !st.GateReady || st.Build < featurelevel.GatewayEndpoints || !agree {
		level = featurelevel.GatewayEndpoints - 1
	}
	if prev := d.advertisedLevel.Swap(int64(level)); prev != int64(level) {
		slog.Info("Feature level advertised", "level", level, "build", featurelevel.Build,
			"capture_probe", st.ProbePassed, "gate_ready", st.GateReady, "vpcd_build", st.Build,
			"prefix_list_derivations_agree", agree, "err", err)
	}
	return level
}

// prefixListDerivationsAgree reports whether the gossiped node set gives this
// node its own IPv4 entry, and its entries derived with its bridge address equal
// those every node derives from that set, which level 2 switches to.
func prefixListDerivationsAgree(cc *config.ClusterConfig, mgmtBridgeIP string) bool {
	own := cc != nil && slices.ContainsFunc(config.ServiceEndpoints(cc, ""), func(ep config.ServiceEndpoint) bool {
		return ep.Node == cc.Node && ep.Addr.Is4()
	})
	return own && slices.Equal(prefixlist.Entries(cc, mgmtBridgeIP), prefixlist.Entries(cc, ""))
}

// prefixListGossiped reports whether level 2 is active, for the prefix-list
// reads; an unreadable level keeps the local derivation, which agreed at
// activation.
func prefixListGossiped(gate *featurelevel.Gate) func(context.Context) bool {
	return func(ctx context.Context) bool {
		if gate == nil {
			return false
		}
		ok, err := gate.Active(ctx, featurelevel.GatewayEndpoints)
		return err == nil && ok
	}
}

// prefixListAdmission admits security-group writes adding a prefix-list peer.
func prefixListAdmission(gate *featurelevel.Gate) func(context.Context) (func(), bool, error) {
	return func(ctx context.Context) (func(), bool, error) {
		if gate == nil {
			return func() {}, false, errors.New("feature level gate not ready")
		}
		return gate.Admit(ctx, featurelevel.GatewayEndpoints)
	}
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
