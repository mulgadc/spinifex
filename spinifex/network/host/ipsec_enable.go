package host

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// systemctlActiveTimeout bounds the wait for openvswitch-ipsec.service to become active.
var systemctlActiveTimeout = 5 * time.Second

// ovnNBSocketPath gates the NB_Global ipsec write to the management node.
var ovnNBSocketPath = "/run/ovn/ovnnb_db.sock"

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

// ReconcileOVNIPSec brings the host's IPsec services in line with the cluster
// config, then enables OVN IPsec if it is wanted. Runs on every startup so the
// disabled case is reached too, which EnableOVNIPSec alone never is.
func ReconcileOVNIPSec(configPath string, clusterConfig *config.ClusterConfig) error {
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
	return EnableOVNIPSec(configPath, clusterConfig)
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
func EnableOVNIPSec(configPath string, clusterConfig *config.ClusterConfig) error {
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

	// NB_Global.ipsec is cluster-wide; only the management node has a local NB socket.
	// Without this flag, ovn-controller skips adding options:remote_name to Geneve
	// tunnels and ovs-monitor-ipsec never materialises strongSwan connections.
	if _, err := os.Stat(ovnNBSocketPath); err == nil {
		if err := SetNBGlobalIPSec(true); err != nil {
			return err
		}
	}

	slog.Info("OVN native IPsec enabled on intra-AZ Geneve",
		"cert", certPath,
		"key", keyPath,
		"ca", caCertPath,
	)
	return nil
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
