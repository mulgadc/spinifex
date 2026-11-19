package daemon

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/prefixlist"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	"github.com/mulgadc/spinifex/spinifex/foundation/lifecycle/reconciler"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
)

// endpointDemuxer returns the plumber's demux half, or nil when it has none,
// which leaves the gateway-endpoint demux off.
func (d *Daemon) endpointDemuxer() vm.EndpointDemuxer {
	dm, _ := d.networkPlumber.(vm.EndpointDemuxer)
	return dm
}

// endpointIntent derives this node's demux intent from the projection: the
// subnets routed to an endpoint, and the prefix-list entries they reach it at.
func (d *Daemon) endpointIntent(ctx context.Context) (vm.EndpointIntent, error) {
	if d.jsManager == nil || d.jsManager.js == nil {
		return vm.EndpointIntent{}, errors.New("no JetStream connection")
	}
	eps, err := projection.LoadGatewayEndpoints(ctx, d.jsManager.js)
	if err != nil {
		return vm.EndpointIntent{}, err
	}
	in := vm.EndpointIntent{Subnets: make(map[string]bool, len(eps))}
	for subnet := range eps {
		in.Subnets[subnet] = true
	}
	if len(in.Subnets) == 0 {
		return in, nil
	}
	in.Entries = d.endpointEntries(ctx)
	return in, nil
}

// endpointEntries lists the gate addresses the demux matches: while level 2
// holds, the gossiped derivation vpcd's capture uses, else this node's view.
func (d *Daemon) endpointEntries(ctx context.Context) []netip.AddrPort {
	mgmt := d.mgmtBridgeIP
	if prefixListGossiped(d.featureGate)(ctx) {
		mgmt = ""
	}
	var out []netip.AddrPort
	for _, e := range prefixlist.Entries(d.clusterConfig, mgmt) {
		out = append(out, netip.AddrPortFrom(e.Addr, e.GatePort))
	}
	return out
}

// startEndpointDemuxReconcile runs the demux pass on every endpoint, route
// table or subnet change, and on the IMDS reconcile interval, which bounds how
// long a guest that starts, or a demux an ovs-vswitchd restart dropped, waits.
func (d *Daemon) startEndpointDemuxReconcile(ctx context.Context) {
	if d.endpointDemuxer() == nil || d.jsManager == nil || d.jsManager.js == nil {
		return
	}
	sources := make([]reconciler.Source, len(projection.EndpointBuckets))
	for i, name := range projection.EndpointBuckets {
		b := kvstore.NewBucket(d.jsManager.js, kvstore.Config{Name: name, AttachOnly: true})
		sources[i] = reconciler.Fixed(b, ">")
	}
	go reconciler.Run(ctx, reconciler.Config{
		Name:    "endpoint-demux",
		Sources: sources,
		// A failed pass changes no flow; the loop logs it and retries.
		Reconcile: func(ctx context.Context) (time.Duration, error) {
			return 0, d.vmMgr.ReconcileEndpointDemux(ctx)
		},
		Resync: vm.IMDSDatapathReconcileInterval,
	})
}
