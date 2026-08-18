package daemon

import (
	"github.com/mulgadc/spinifex/spinifex/otelsetup"
	"log/slog"
	"time"
)

const heartbeatInterval = 10 * time.Second

// startHeartbeat launches a goroutine that publishes this daemon's heartbeat
// to the cluster-state KV store every heartbeatInterval. It fires immediately
// on start, then repeats on a ticker. The goroutine exits when d.ctx is cancelled.
func (d *Daemon) startHeartbeat() {
	if d.jsManager == nil {
		slog.Warn("JetStream not initialized, skipping heartbeat")
		return
	}

	go func() {
		// Fire immediately on startup
		d.publishHeartbeat()

		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()

		for {
			select {
			case <-d.ctx.Done():
				slog.Info("Heartbeat goroutine stopping")
				return
			case <-ticker.C:
				d.publishHeartbeat()
			}
		}
	}()

	slog.Info("Heartbeat started", "interval_ms", otelsetup.Millis(heartbeatInterval))
}

// publishHeartbeat builds and writes a heartbeat entry to KV.
func (d *Daemon) publishHeartbeat() {
	h := d.buildHeartbeat()
	if err := d.jsManager.WriteHeartbeat(h); err != nil {
		slog.Warn("Failed to publish heartbeat", "error", err)
	} else {
		slog.Debug("Heartbeat published", "node", h.Node, "vms", h.VMCount)
	}
}

// buildHeartbeat constructs a Heartbeat from the daemon's current state.
// AvailableVCPU / AvailableMem retain "host - allocated" semantics for
// observability continuity; the daemon's reserve is surfaced separately
// so consumers can compute schedulable capacity.
func (d *Daemon) buildHeartbeat() *Heartbeat {
	totalVCPU, totalMem, reservedVCPU, reservedMem, allocVCPU, allocMem, _ := d.resourceMgr.GetResourceStats()

	vmCount := d.vmMgr.Count()

	return &Heartbeat{
		Node:          d.node,
		Epoch:         d.clusterConfig.Epoch,
		Timestamp:     time.Now().UTC().Format(time.RFC3339),
		Services:      d.config.GetServices(),
		VMCount:       vmCount,
		AllocatedVCPU: allocVCPU,
		AvailableVCPU: totalVCPU - allocVCPU,
		AllocatedMem:  allocMem,
		AvailableMem:  totalMem - allocMem,
		ReservedVCPU:  reservedVCPU,
		ReservedMem:   reservedMem,
	}
}
