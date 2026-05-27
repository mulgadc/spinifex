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
		out, _ := utils.SudoCommand("systemctl", "is-active", "openvswitch-ipsec.service").CombinedOutput()
		lastOut = strings.TrimSpace(string(out))
		if lastOut == "active" {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("not active after %s: %s (provision via scripts/setup-ovn.sh)", systemctlActiveTimeout, lastOut)
}
