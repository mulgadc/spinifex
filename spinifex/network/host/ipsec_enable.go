package host

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/otelsetup"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// systemctlActiveTimeout bounds the wait for openvswitch-ipsec.service to become active.
var systemctlActiveTimeout = 5 * time.Second

// ipsecRetryDelay is the first gap after a failed pass; it doubles up to
// ipsecReconcileInterval. Short, because until every chassis has finished its
// local setup the cluster cannot require encryption at all.
var ipsecRetryDelay = 3 * time.Second

// ipsecReconcileInterval re-runs a successful pass. It doubles as the readiness
// heartbeat, so it must stay well inside whatever freshness window the barrier
// applies to the records this publishes.
var ipsecReconcileInterval = 60 * time.Second

// IPSecBarrier carries each node's local IPsec completion across the cluster.
// NB_Global.ipsec is cluster-wide, so asserting it from one node's local
// knowledge is what lets a partially configured mesh black-hole guest traffic.
// The interface keeps the transport (JetStream KV) out of L0.
type IPSecBarrier interface {
	// PublishLocalReady records whether this node's own IPsec configuration is
	// complete. Callers re-publish on every pass, so records stay fresh.
	PublishLocalReady(ctx context.Context, node string, ready bool) error

	// NodesReady reports whether every named node has published readiness, and
	// names those that have not.
	NodesReady(ctx context.Context, nodes []string) (bool, []string, error)
}

const (
	// ovsIPSecUnit owns charon: ovs-monitor-ipsec execs the strongSwan starter
	// itself rather than going through the starter's own unit.
	ovsIPSecUnit = "openvswitch-ipsec.service"

	// strongswanStarterUnit would start a second charon competing for UDP
	// 500/4500. Nothing here needs it, so it stays off in both states.
	strongswanStarterUnit = "strongswan-starter.service"
)

// ipsecStateHelper is the fixed-verb root helper installed by setup.sh. The
// daemon holds no systemctl grant, and a NOPASSWD rule for one would be
// root-equivalent, so unit changes go through this instead.
var ipsecStateHelper = "/usr/local/lib/spinifex/spinifex-set-ipsec-state"

// MaintainIPSec schedules ReconcileOVNIPSec until it succeeds, then re-runs it
// at ipsecReconcileInterval for the lifetime of ctx. A scheduler only.
//
// A single attempt at startup routinely loses the race with ovn-central
// accepting connections, and dropping that attempt leaves the node's IPsec
// unconfigured until something restarts the daemon — while another node may
// already have made encryption mandatory cluster-wide.
func MaintainIPSec(ctx context.Context, configPath string, clusterConfig *config.ClusterConfig, barrier IPSecBarrier) {
	delay := ipsecRetryDelay
	for {
		if err := ReconcileOVNIPSec(ctx, configPath, clusterConfig, barrier); err != nil {
			slog.Warn("Failed to reconcile OVN native IPsec, will retry", "err", err, "retry_in_ms", otelsetup.Millis(delay))
			delay = min(delay*2, ipsecReconcileInterval)
		} else {
			delay = ipsecReconcileInterval
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// ReconcileOVNIPSec brings the host's IPsec services in line with the cluster
// config, then enables OVN IPsec if it is wanted. Runs on every startup so the
// disabled case is reached too, which EnableOVNIPSec alone never is.
func ReconcileOVNIPSec(ctx context.Context, configPath string, clusterConfig *config.ClusterConfig, barrier IPSecBarrier) error {
	// A nil config means the intent is unknown; leave the host's services alone
	// rather than guessing and tearing down working tunnels.
	if clusterConfig == nil {
		return nil
	}

	want := clusterConfig.Network.IPSecEnabled && len(clusterConfig.Nodes) > 1

	if err := ensureIPSecServices(want); err != nil {
		return err
	}
	if !want {
		slog.Info("ipsec: not in use, IKE and NAT-T listeners stopped")
		return nil
	}
	return EnableOVNIPSec(ctx, configPath, clusterConfig, barrier)
}

// ensureIPSecServices runs the helper only when the host does not already match
// the wanted state, so a steady-state startup executes nothing.
func ensureIPSecServices(want bool) error {
	if ipsecServicesMatch(want) {
		return nil
	}

	state := "off"
	if want {
		state = "on"
	}
	out, err := utils.SudoCommand(ipsecStateHelper, state).CombinedOutput()
	if err != nil {
		return fmt.Errorf("set ipsec state %s: %w: %s", state, err, strings.TrimSpace(string(out)))
	}
	slog.Info("ipsec: host services reconciled", "state", state)
	return nil
}

func ipsecServicesMatch(want bool) bool {
	if unitIsEnabled(strongswanStarterUnit) || unitIsActive(strongswanStarterUnit) {
		return false
	}
	return unitIsEnabled(ovsIPSecUnit) == want && unitIsActive(ovsIPSecUnit) == want
}

// unitIsEnabled treats every state other than enabled as not enabled, which
// covers masked, static and a unit the distro does not ship at all. The helper
// masks rather than disables, so the off state reads back as "masked".
func unitIsEnabled(unit string) bool {
	out, _ := utils.SudoCommand("systemctl", "is-enabled", unit).CombinedOutput()
	state := strings.TrimSpace(string(out))
	return state == "enabled" || state == "enabled-runtime" || state == "alias"
}

func unitIsActive(unit string) bool {
	out, _ := utils.SudoCommand("systemctl", "is-active", unit).CombinedOutput()
	return strings.TrimSpace(string(out)) == "active"
}

// EnableOVNIPSec wires the local IPsec peer cert and flips ipsec_encapsulation=true.
// Idempotent. Single-node clusters short-circuit (no Geneve tunnels to encrypt).
// Lives in L0 per ADR-0006 S8 (IPSec is OVN-native only; SA lifecycle invisible above L0).
func EnableOVNIPSec(ctx context.Context, configPath string, clusterConfig *config.ClusterConfig, barrier IPSecBarrier) error {
	if configPath == "" {
		return fmt.Errorf("config path unset")
	}
	if clusterConfig != nil && len(clusterConfig.Nodes) <= 1 {
		slog.Info("ipsec: single-node cluster, skipping enable (no peers)")
		return nil
	}
	configDir := filepath.Dir(configPath)
	certPath, keyPath := admin.IPSecCertPaths(configDir)
	caCertPath := filepath.Join(configDir, "ca.pem")

	for _, p := range []string{certPath, keyPath, caCertPath} {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("missing IPsec credential %s: %w", p, err)
		}
	}

	if err := ensureOVSMonitorIPSecActive(); err != nil {
		return fmt.Errorf("ovs-monitor-ipsec: %w", err)
	}

	if err := SetIPSecCertPaths(certPath, keyPath, caCertPath); err != nil {
		return err
	}

	if err := EnableIPSecEncapsulation(); err != nil {
		return err
	}

	// Published before the cluster-wide flag is touched: this node is now safe to
	// send and receive encrypted Geneve, whoever ends up asserting it.
	if barrier != nil && clusterConfig != nil {
		if err := barrier.PublishLocalReady(ctx, clusterConfig.Node, true); err != nil {
			return fmt.Errorf("publish IPsec readiness: %w", err)
		}
	}

	slog.Info("OVN native IPsec enabled on intra-AZ Geneve",
		"cert", certPath,
		"key", keyPath,
		"ca", caCertPath,
	)
	return reconcileNBGlobalIPSec(ctx, clusterConfig, barrier)
}

// reconcileNBGlobalIPSec holds NB_Global.ipsec in step with the whole cluster's
// readiness. Without this flag ovn-controller skips options:remote_name on
// Geneve tunnels and ovs-monitor-ipsec materialises no strongSwan connections;
// with it, a chassis that has not finished its own setup silently drops guest
// traffic. So it tracks the slowest chassis, not this one.
func reconcileNBGlobalIPSec(ctx context.Context, clusterConfig *config.ClusterConfig, barrier IPSecBarrier) error {
	// Reachability, role and current value in one call. A stat of the socket file
	// answers none of them: in a clustered OVN the file exists on every node long
	// before the database behind it accepts a connection.
	current, err := GetNBGlobalIPSec()
	if err != nil {
		slog.Debug("ipsec: local OVN NB DB unreachable, leaving NB_Global to a node that can read it", "err", err)
		return nil
	}

	if barrier == nil || clusterConfig == nil {
		if current {
			return nil
		}
		return SetNBGlobalIPSec(true)
	}

	ready, pending, err := barrier.NodesReady(ctx, nodeNames(clusterConfig))
	if err != nil {
		// Unreadable is not evidence that a chassis is unconfigured. Retracting on
		// it would drop a working encrypted mesh to plaintext over a KV outage.
		return fmt.Errorf("read IPsec readiness: %w", err)
	}

	switch {
	case ready && current:
		return nil
	case ready:
		slog.Info("ipsec: every chassis reports a complete configuration, requiring encryption cluster-wide")
		return SetNBGlobalIPSec(true)
	case !current:
		slog.Info("ipsec: holding encryption off until every chassis is configured", "pending", pending)
		return nil
	}

	// Plaintext Geneve is where the cluster sat before IPsec was asked for, and it
	// is both recoverable and visible. A black hole is neither.
	slog.Error("ipsec: retracting cluster-wide encryption, chassis are unconfigured and cross-chassis guest traffic would black-hole", "pending", pending)
	return SetNBGlobalIPSec(false)
}

// nodeNames returns cluster membership in a stable order so logged pending sets
// do not churn between passes.
func nodeNames(clusterConfig *config.ClusterConfig) []string {
	return slices.Sorted(maps.Keys(clusterConfig.Nodes))
}

// ensureOVSMonitorIPSecActive polls openvswitch-ipsec.service for "active".
// If inactive, refuse to flip ipsec_encapsulation=true (would silently drop traffic).
func ensureOVSMonitorIPSecActive() error {
	deadline := time.Now().Add(systemctlActiveTimeout)
	var lastOut string
	for time.Now().Before(deadline) {
		out, _ := utils.SudoCommand("systemctl", "is-active", ovsIPSecUnit).CombinedOutput()
		lastOut = strings.TrimSpace(string(out))
		if lastOut == "active" {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("not active after %s: %s (provision via scripts/setup-ovn.sh)", systemctlActiveTimeout, lastOut)
}
