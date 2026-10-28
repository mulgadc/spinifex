package vpcd

import (
	"context"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/network/host"
	"github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
)

// mssClampInterval bounds how long a firewall reload or flush can leave the
// uplink unclamped; the nft batch is idempotent, so a pass costs one exec.
var mssClampInterval = 30 * time.Second

// runMSSClampLoop installs the host MSS clamp now and re-asserts it until ctx
// ends. Every node runs it: the clamp lives in this host's own datapath.
func runMSSClampLoop(ctx context.Context, r host.Runner, mode host.UplinkMode, gatewayMTU int) {
	slog.Info("vpcd: MSS clamp loop started", "mode", mode, "gateway_mtu", gatewayMTU,
		"interval_ms", otelsetup.Millis(mssClampInterval))

	ticker := time.NewTicker(mssClampInterval)
	defer ticker.Stop()
	for first := true; ; first = false {
		if err := host.EnsureMSSClamp(ctx, r, mode, gatewayMTU); err != nil && ctx.Err() == nil {
			// A failed start leaves oversized TCP black-holing, so it is an error; a
			// failed re-assert only means the last install still stands.
			if first {
				slog.Error("vpcd: MSS clamp install failed", "mode", mode, "err", err)
			} else {
				slog.Warn("vpcd: MSS clamp re-assert failed", "mode", mode, "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
