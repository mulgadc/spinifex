package host

import (
	"context"
	"errors"
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

// IPSecNodeStatus is one node's published view of its own IPsec state.
type IPSecNodeStatus struct {
	// Ready reports that this node's local IPsec configuration is complete.
	Ready bool

	// NBReachable reports that this node can read and write NB_Global. Only
	// management nodes can, and which nodes those are is not in the config, so
	// they say so here and every node elects the same writer from the answer.
	NBReachable bool
}

// IPSecBarrier carries each node's IPsec state across the cluster.
// NB_Global.ipsec is cluster-wide, so asserting it from one node's local
// knowledge is what lets a partially configured mesh black-hole guest traffic.
// The interface keeps the transport (JetStream KV) out of L0.
type IPSecBarrier interface {
	// Publish records this node's own state. Callers publish on every pass, so
	// a live node's record stays fresh and a stopped one's goes stale.
	Publish(ctx context.Context, node string, status IPSecNodeStatus) error

	// Cluster returns the last published status of every named node that is
	// still live. A node that is not live is absent from the map rather than
	// unready: it has no chassis registered, so there is no tunnel to it to
	// black-hole, and demanding plaintext on its behalf would only strip
	// encryption from the traffic that is still flowing.
	Cluster(ctx context.Context, nodes []string) (map[string]IPSecNodeStatus, error)
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
	// Said once, because a pass that reconciles to nothing logs nothing, which in
	// a journal is indistinguishable from a node that armed.
	switch {
	case clusterConfig == nil:
		slog.Info("ipsec: cluster membership unknown, leaving the host's IPsec services alone")
	case !ipsecWanted(clusterConfig):
		slog.Info("ipsec: not in use on this cluster, IKE and NAT-T listeners stay stopped")
	}

	delay, announced := ipsecRetryDelay, false
	for {
		err := ReconcileOVNIPSec(ctx, configPath, clusterConfig, barrier)
		switch {
		case err != nil:
			delay = min(delay*2, ipsecReconcileInterval)
			slog.Warn("Failed to reconcile OVN native IPsec, will retry", "err", err, "retry_in_ms", otelsetup.Millis(delay))
		default:
			// Only the first success is news. The steady state re-runs every
			// interval for the lifetime of the daemon, and saying so each time
			// buries the passes that changed something.
			if !announced && ipsecWanted(clusterConfig) {
				slog.Info("ipsec: local IPsec configuration complete on this node")
				announced = true
			}
			delay = ipsecReconcileInterval
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// ipsecWanted reports the cluster's intent. Single-node clusters have no Geneve
// tunnels to protect, so charon must not be left listening on 500/4500 there
// even though ipsec_enabled defaults to true.
func ipsecWanted(clusterConfig *config.ClusterConfig) bool {
	return clusterConfig != nil && clusterConfig.Network.IPSecEnabled && len(clusterConfig.Nodes) > 1
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

	want := ipsecWanted(clusterConfig)

	if err := ensureIPSecServices(want); err != nil {
		return err
	}
	if !want {
		return DisableOVNIPSec(ctx, clusterConfig, barrier)
	}
	return EnableOVNIPSec(ctx, configPath, clusterConfig, barrier)
}

// DisableOVNIPSec is the off direction, and it has to reach NB_Global. Charon is
// already stopped by the time this runs, so a flag left asserted demands
// encryption that no chassis can perform — the same black hole as a partial
// enable, reached by turning IPsec off instead of on.
func DisableOVNIPSec(ctx context.Context, clusterConfig *config.ClusterConfig, barrier IPSecBarrier) error {
	if barrier != nil && clusterConfig != nil {
		if err := barrier.Publish(ctx, clusterConfig.Node, IPSecNodeStatus{}); err != nil {
			return fmt.Errorf("publish IPsec state: %w", err)
		}
	}

	current, err := GetNBGlobalIPSec()
	if err != nil {
		return skipUnlessNBReadable(err)
	}
	if !current {
		return nil
	}
	slog.Info("ipsec: switched off for this cluster, releasing the cluster-wide encryption requirement")
	return SetNBGlobalIPSec(false)
}

// skipUnlessNBReadable turns "this node has no local NB DB" into a quiet skip
// and leaves every other failure — a socket it may not open, a missing binary,
// a timed-out transaction — as a real error, so the pass retries and says so.
func skipUnlessNBReadable(err error) error {
	if errors.Is(err, errNBUnreachable) {
		slog.Debug("ipsec: no local OVN NB DB, leaving NB_Global to a management node", "err", err)
		return nil
	}
	return err
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
		slog.Debug("ipsec: single-node cluster, skipping enable (no peers)")
		return nil
	}

	// Probed before the local half, because the answer is published alongside it:
	// which nodes can write NB_Global is not in the config, so they have to say.
	current, nbErr := GetNBGlobalIPSec()
	if nbErr != nil && !errors.Is(nbErr, errNBUnreachable) {
		return nbErr
	}
	status := IPSecNodeStatus{NBReachable: nbErr == nil}

	if err := configureLocalIPSec(configPath); err != nil {
		// Said out loud, not left to expire. A record that only goes stale keeps
		// this chassis counted as configured for a whole freshness window, which
		// is the black hole again with a slower fuse.
		publishStatus(ctx, clusterConfig, barrier, status)
		return err
	}
	status.Ready = true

	if barrier != nil && clusterConfig != nil {
		if err := barrier.Publish(ctx, clusterConfig.Node, status); err != nil {
			return fmt.Errorf("publish IPsec state: %w", err)
		}
	}

	if nbErr != nil {
		return skipUnlessNBReadable(nbErr)
	}
	return reconcileNBGlobalIPSec(ctx, clusterConfig, barrier, current)
}

// configureLocalIPSec does this node's own half: point OVS at the peer cert and
// turn on encapsulation, once ovs-monitor-ipsec is up to act on both.
func configureLocalIPSec(configPath string) error {
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
	return EnableIPSecEncapsulation()
}

// publishStatus is the best-effort form used on failure paths, where the error
// being reported matters more than the one this might add.
func publishStatus(ctx context.Context, clusterConfig *config.ClusterConfig, barrier IPSecBarrier, status IPSecNodeStatus) {
	if barrier == nil || clusterConfig == nil {
		return
	}
	if err := barrier.Publish(ctx, clusterConfig.Node, status); err != nil {
		slog.Warn("ipsec: could not publish this node's state, peers will wait for it to go stale", "err", err)
	}
}

// reconcileNBGlobalIPSec holds NB_Global.ipsec in step with the whole cluster.
// Without this flag ovn-controller skips options:remote_name on Geneve tunnels
// and ovs-monitor-ipsec materialises no strongSwan connections; with it, a
// chassis that has not finished its own setup silently drops guest traffic. So
// it tracks the slowest live chassis, not this one.
func reconcileNBGlobalIPSec(ctx context.Context, clusterConfig *config.ClusterConfig, barrier IPSecBarrier, current bool) error {
	if barrier == nil || clusterConfig == nil {
		if current {
			return nil
		}
		return SetNBGlobalIPSec(true)
	}

	cluster, err := barrier.Cluster(ctx, nodeNames(clusterConfig))
	if err != nil {
		// Unreadable is not evidence that a chassis is unconfigured. Retracting on
		// it would drop a working encrypted mesh to plaintext over a KV outage.
		return fmt.Errorf("read IPsec cluster state: %w", err)
	}

	// One writer. Every management node can reach the NB DB, so without this they
	// race on one cluster-global row from snapshots taken at different instants,
	// and two that disagree for a single pass flip the flag — tearing down and
	// rebuilding every strongSwan connection in the cluster on each flip.
	writer := nbGlobalWriter(cluster)
	if writer != clusterConfig.Node {
		slog.Debug("ipsec: another node owns the NB_Global write this pass", "writer", writer)
		return nil
	}

	pending := notReady(cluster)
	switch {
	case len(pending) == 0 && current:
		return nil
	case len(pending) == 0:
		slog.Info("ipsec: every live chassis reports a complete configuration, requiring encryption cluster-wide")
		return SetNBGlobalIPSec(true)
	case !current:
		slog.Info("ipsec: holding encryption off until every chassis is configured", "pending", pending)
		return nil
	}

	// Plaintext Geneve is where the cluster sat before IPsec was asked for, and it
	// is both recoverable and visible. A black hole is neither.
	slog.Error("ipsec: retracting cluster-wide encryption, chassis are unconfigured and cross-chassis guest traffic would black-hole", "pending", pending)
	if err := SetNBGlobalIPSec(false); err != nil {
		slog.Error("ipsec: NB_Global still demands encryption that unconfigured chassis cannot perform, cross-chassis guest traffic is being dropped now",
			"pending", pending, "err", err)
		return err
	}
	return nil
}

// nbGlobalWriter picks the one node that may write NB_Global this pass: the
// first live management node in name order. Every node computes it from the same
// published set, so they agree without a lock.
func nbGlobalWriter(cluster map[string]IPSecNodeStatus) string {
	var writer string
	for _, node := range slices.Sorted(maps.Keys(cluster)) {
		if cluster[node].NBReachable {
			writer = node
			break
		}
	}
	return writer
}

// notReady names the live nodes whose own IPsec configuration is incomplete.
func notReady(cluster map[string]IPSecNodeStatus) []string {
	var pending []string
	for _, node := range slices.Sorted(maps.Keys(cluster)) {
		if !cluster[node].Ready {
			pending = append(pending, node)
		}
	}
	return pending
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
