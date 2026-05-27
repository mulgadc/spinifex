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

// systemctlActiveTimeout bounds how long EnableOVNIPSec waits for
// openvswitch-ipsec.service to report active. The unit is enabled at
// provision time (scripts/setup-ovn.sh) so the daemon only needs read
// capability (systemctl is-active); this poll absorbs boot-time races
// where the unit hasn't finished starting yet. Overridden in tests.
var systemctlActiveTimeout = 5 * time.Second

// ovnNBSocketPath gates the NB_Global ipsec=true write to the management
// node — workers don't run ovn-central and have no local socket.
// Overridden in tests.
var ovnNBSocketPath = "/run/ovn/ovnnb_db.sock"

// EnableOVNIPSec wires the per-node IPsec peer cert into the local OVS DB and
// flips ipsec_encapsulation=true. ovs-monitor-ipsec (shipped by the
// openvswitch-ipsec package) reads the cert/key/CA pointers and materialises
// a strongSwan config for every Geneve tunnel that ovn-controller programs.
//
// Idempotent: ovs-vsctl set is repeatable; on restart the values are
// overwritten with the same paths.
//
// Caller is expected to gate on clusterConfig.Network.IPSecEnabled. If the
// admin init / admin join step never produced an IPsec peer cert (e.g.
// because the cluster was bootstrapped before this feature landed), the
// function returns an error so the daemon can log it and continue; the
// cluster keeps running with plaintext Geneve until an operator reissues
// certs.
//
// Single-node clusters short-circuit: with no peers, there are no Geneve
// tunnels to encrypt, and flipping ipsec_encapsulation=true on a host where
// ovs-monitor-ipsec is absent or dead would silently drop tunnel traffic.
//
// Lives in L0 (network/host) per ADR-0006 S8: "IPSec is OVN-native only…
// IPSec SA lifecycle is delegated entirely to OVN native IPSec and is
// invisible above L0." The daemon only chooses whether to invoke it; the
// orchestration sequence (verify monitor → set cert pointers → enable
// encapsulation → flip NB_Global) belongs in L0.
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

	// NB_Global.ipsec=true triggers ovn-controller on every chassis to add
	// options:remote_name to the Geneve tunnel ports, which is what
	// ovs-monitor-ipsec keys off to materialise the per-peer strongSwan
	// connections. Without it, xfrm stays empty and Geneve runs plaintext
	// even with ipsec_encapsulation=true set locally. Only the management
	// node has a local NB socket; on workers, skip silently — the flag is
	// cluster-wide and one writer is enough.
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
// The unit is enabled at provision time (scripts/setup-ovn.sh); the daemon
// has read-only sudoers scope (systemctl is-active). If the unit is inactive
// here, operator intervention is required — daemon refuses to flip
// ipsec_encapsulation=true and silently drop tunnel traffic.
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
