package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// pidDir returns the directory containing shell-level PID files for this node.
// start-dev.sh writes PID files to $DATA_DIR/logs/ and the daemon's BaseDir is
// always $DATA_DIR/spinifex/, so we derive the logs directory from that. Each node
// has its own DATA_DIR, so this is safe for simulated multi-node on a single host.
func (d *Daemon) pidDir() string {
	if d.config.BaseDir != "" {
		return filepath.Join(filepath.Dir(filepath.Clean(d.config.BaseDir)), "logs")
	}
	return ""
}

// ShutdownRequest is sent by the coordinator to each daemon per phase.
type ShutdownRequest struct {
	Phase   string `json:"phase"`
	Force   bool   `json:"force"`
	Timeout int    `json:"timeout_seconds"`
}

// ShutdownACK is the response from a daemon after completing a shutdown phase.
type ShutdownACK struct {
	Node    string   `json:"node"`
	Phase   string   `json:"phase"`
	Stopped []string `json:"stopped,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// ShutdownProgress is published by daemons during the DRAIN phase to report VM shutdown progress.
type ShutdownProgress struct {
	Node      string `json:"node"`
	Phase     string `json:"phase"`
	Total     int    `json:"total"`
	Remaining int    `json:"remaining"`
}

// handleShutdownGate stops the API gateway and UI, then sets shuttingDown flag
// so the daemon rejects new work. Phase: GATE.
func (d *Daemon) handleShutdownGate(msg *nats.Msg) {
	var req ShutdownRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		slog.Error("handleShutdownGate: failed to unmarshal request", "error", err)
		d.respondShutdownACK(msg, ShutdownACK{Node: d.node, Phase: "gate", Error: err.Error()})
		return
	}

	slog.Info("Shutdown GATE phase starting", "node", d.node, "force", req.Force)

	var stopped []string

	// Stop AWSGW
	if d.config.HasService("awsgw") {
		if err := utils.StopProcessAt(d.pidDir(), "awsgw"); err != nil {
			slog.Warn("Failed to stop awsgw", "error", err)
		} else {
			stopped = append(stopped, "awsgw")
		}
	}

	// Stop UI
	if d.config.HasService("ui") {
		if err := utils.StopProcessAt(d.pidDir(), "spinifex-ui"); err != nil {
			slog.Warn("Failed to stop spinifex-ui", "error", err)
		} else {
			stopped = append(stopped, "spinifex-ui")
		}
	}

	// Stop vpcd (VPC daemon)
	if d.config.HasService("vpcd") {
		if err := utils.StopProcessAt(d.pidDir(), "vpcd"); err != nil {
			slog.Warn("Failed to stop vpcd", "error", err)
		} else {
			stopped = append(stopped, "vpcd")
		}
	}

	// Set shuttingDown flag so daemon rejects new work
	d.shuttingDown.Store(true)

	ack := ShutdownACK{
		Node:    d.node,
		Phase:   "gate",
		Stopped: stopped,
	}
	d.respondShutdownACK(msg, ack)
	slog.Info("Shutdown GATE phase complete", "node", d.node, "stopped", stopped)
}

// handleShutdownDrain gracefully stops all running VMs, writes shutdown marker
// and persists state. Phase: DRAIN.
func (d *Daemon) handleShutdownDrain(msg *nats.Msg) {
	var req ShutdownRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		slog.Error("handleShutdownDrain: failed to unmarshal request", "error", err)
		d.respondShutdownACK(msg, ShutdownACK{Node: d.node, Phase: "drain", Error: err.Error()})
		return
	}

	slog.Info("Shutdown DRAIN phase starting", "node", d.node)

	total := d.vmMgr.Count()

	// Publish initial progress
	d.publishShutdownProgress("drain", total, total)

	// Stop all instances (graceful shutdown, no volume deletion). StopAll
	// fans out per-VM shutdown internally; the manager's snapshot decouples
	// iteration from concurrent terminate handlers.
	if total > 0 {
		if err := d.vmMgr.StopAll(); err != nil {
			slog.Error("Failed to stop instances during DRAIN", "error", err)
			ack := ShutdownACK{
				Node:  d.node,
				Phase: "drain",
				Error: err.Error(),
			}
			d.respondShutdownACK(msg, ack)
			return
		}
	}

	// Publish final progress
	d.publishShutdownProgress("drain", total, 0)

	// Write shutdown marker and persist state
	if d.jsManager != nil {
		if err := d.jsManager.WriteShutdownMarker(d.node); err != nil {
			slog.Error("Failed to write shutdown marker during DRAIN", "error", err)
		}
	}
	if err := d.WriteState(); err != nil {
		slog.Error("Failed to write state during DRAIN", "error", err)
	}

	ack := ShutdownACK{
		Node:  d.node,
		Phase: "drain",
	}
	d.respondShutdownACK(msg, ack)
	slog.Info("Shutdown DRAIN phase complete", "node", d.node, "vms_stopped", total)
}

// handleShutdownStorage stops viperblock and cleans up orphan nbdkit processes. Phase: STORAGE.
func (d *Daemon) handleShutdownStorage(msg *nats.Msg) {
	var req ShutdownRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		slog.Error("handleShutdownStorage: failed to unmarshal request", "error", err)
		d.respondShutdownACK(msg, ShutdownACK{Node: d.node, Phase: "storage", Error: err.Error()})
		return
	}

	slog.Info("Shutdown STORAGE phase starting", "node", d.node)

	var stopped []string

	if d.config.HasService("viperblock") {
		if err := utils.StopProcessAt(d.pidDir(), "viperblock"); err != nil {
			slog.Warn("Failed to stop viperblock", "error", err)
		} else {
			stopped = append(stopped, "viperblock")
		}
	}

	// Best-effort cleanup of orphaned nbdkit processes
	if err := exec.Command("pkill", "-f", "nbdkit").Run(); err != nil {
		slog.Debug("pkill nbdkit (best-effort)", "result", err)
	}

	ack := ShutdownACK{
		Node:    d.node,
		Phase:   "storage",
		Stopped: stopped,
	}
	d.respondShutdownACK(msg, ack)
	slog.Info("Shutdown STORAGE phase complete", "node", d.node, "stopped", stopped)
}

// handleShutdownPersist stops predastore. Phase: PERSIST.
func (d *Daemon) handleShutdownPersist(msg *nats.Msg) {
	var req ShutdownRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		slog.Error("handleShutdownPersist: failed to unmarshal request", "error", err)
		d.respondShutdownACK(msg, ShutdownACK{Node: d.node, Phase: "persist", Error: err.Error()})
		return
	}

	slog.Info("Shutdown PERSIST phase starting", "node", d.node)

	var stopped []string

	if d.config.HasService("predastore") {
		if err := utils.StopProcessAt(d.pidDir(), "predastore"); err != nil {
			slog.Warn("Failed to stop predastore", "error", err)
		} else {
			stopped = append(stopped, "predastore")
		}
	}

	ack := ShutdownACK{
		Node:    d.node,
		Phase:   "persist",
		Stopped: stopped,
	}
	d.respondShutdownACK(msg, ack)
	slog.Info("Shutdown PERSIST phase complete", "node", d.node, "stopped", stopped)
}

// handleShutdownInfra stops NATS and exits the daemon. Phase: INFRA.
// No ACK is sent because NATS is going down — this is fire-and-forget from the coordinator.
func (d *Daemon) handleShutdownInfra(msg *nats.Msg) {
	var req ShutdownRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		slog.Error("handleShutdownInfra: failed to unmarshal request", "error", err)
		d.respondShutdownACK(msg, ShutdownACK{Node: d.node, Phase: "infra", Error: err.Error()})
		return
	}

	slog.Info("Shutdown INFRA phase starting", "node", d.node)

	// Cancel context to stop heartbeat and other goroutines
	d.cancel()

	// Unsubscribe from all NATS topics
	for topic, sub := range d.natsSubscriptions {
		slog.Debug("Unsubscribing from NATS", "topic", topic)
		if err := sub.Unsubscribe(); err != nil {
			slog.Warn("Failed to unsubscribe", "topic", topic, "error", err)
		}
	}

	// Shutdown cluster manager
	if d.clusterServer != nil {
		if err := d.clusterServer.Shutdown(context.Background()); err != nil {
			slog.Warn("Failed to shutdown cluster manager", "error", err)
		}
	}

	// Close NATS connection
	d.natsConn.Close()

	// Stop NATS server if this node runs it
	if d.config.HasService("nats") {
		if err := utils.StopProcessAt(d.pidDir(), "nats"); err != nil {
			slog.Warn("Failed to stop nats", "error", err)
		} else {
			slog.Info("NATS server stopped", "node", d.node)
		}
	}

	slog.Info("Shutdown INFRA phase complete, exiting", "node", d.node)

	os.Exit(0)
}

// respondShutdownACK marshals and sends a ShutdownACK response.
func (d *Daemon) respondShutdownACK(msg *nats.Msg, ack ShutdownACK) {
	data, err := json.Marshal(ack)
	if err != nil {
		slog.Error("Failed to marshal shutdown ACK", "error", err)
		return
	}
	if err := msg.Respond(data); err != nil {
		slog.Error("Failed to respond with shutdown ACK", "error", err)
	}
}

// publishShutdownProgress publishes a progress update during the DRAIN phase.
func (d *Daemon) publishShutdownProgress(phase string, total, remaining int) {
	progress := ShutdownProgress{
		Node:      d.node,
		Phase:     phase,
		Total:     total,
		Remaining: remaining,
	}
	data, err := json.Marshal(progress)
	if err != nil {
		slog.Error("Failed to marshal shutdown progress", "error", err)
		return
	}
	if err := d.natsConn.Publish("spinifex.cluster.shutdown.progress", data); err != nil {
		slog.Warn("Failed to publish shutdown progress", "error", err)
	}
}
