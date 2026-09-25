package vpcd

import (
	"context"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/reconcile"
	"github.com/mulgadc/spinifex/spinifex/otelsetup"
)

// Host EIP pass cadence. Shorter than the drift interval because the state it
// repairs is one node's routes: a guest that lands here between passes has no
// inbound path at all until the next one, and the pass is a handful of local
// `ip` calls.
var (
	hostEIPInterval   = 30 * time.Second
	hostEIPStartDelay = 5 * time.Second
)

// runHostEIPLoop re-asserts this node's host-side EIP plumbing until ctx ends.
//
// Every node runs it, unlike the drift loop: the NB rows it reads are shared,
// but the routes and proxy-ARP it writes belong to this host, and the binder
// leaves alone any EIP whose ENI is bound elsewhere. Outside routed NAT the
// pass is a no-op, so the loop is unconditional.
func runHostEIPLoop(ctx context.Context, rec reconcile.Reconciler) {
	slog.Info("vpcd: host EIP loop started", "interval_ms", otelsetup.Millis(hostEIPInterval))

	select {
	case <-ctx.Done():
		return
	case <-time.After(hostEIPStartDelay):
	}

	ticker := time.NewTicker(hostEIPInterval)
	defer ticker.Stop()
	for {
		if err := rec.ReconcileHostEIPs(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("vpcd: host EIP pass failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
