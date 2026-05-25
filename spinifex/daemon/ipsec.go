package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/admin"
)

// systemctlActiveTimeout bounds how long enableOVNIPSec waits for
// openvswitch-ipsec.service to report active. The unit is enabled at
// provision time (scripts/setup-ovn.sh) so the daemon only needs read
// capability (systemctl is-active); this poll absorbs boot-time races
// where the unit hasn't finished starting yet. Overridden in tests.
var systemctlActiveTimeout = 5 * time.Second

// enableOVNIPSec wires the per-node IPsec peer cert into the local OVS DB and
// flips ipsec_encapsulation=true. ovs-monitor-ipsec (shipped by the
// openvswitch-ipsec package) reads the cert/key/CA pointers and materialises
// a strongSwan config for every Geneve tunnel that ovn-controller programs.
//
// Idempotent: ovs-vsctl set is repeatable; on restart the values are
// overwritten with the same paths.
//
// Caller is expected to gate on d.clusterConfig.Network.IPSecEnabled. If the
// admin init / admin join step never produced an IPsec peer cert (e.g.
// because the cluster was bootstrapped before this feature landed), the
// function returns an error so the daemon can log it and continue; the
// cluster keeps running with plaintext Geneve until an operator reissues
// certs.
//
// Single-node clusters short-circuit: with no peers, there are no Geneve
// tunnels to encrypt, and flipping ipsec_encapsulation=true on a host where
// ovs-monitor-ipsec is absent or dead would create the silent-drop trap that
// triggers mulga-siv-136.
func (d *Daemon) enableOVNIPSec() error {
	if d.configPath == "" {
		return fmt.Errorf("config path unset")
	}
	if d.clusterConfig != nil && len(d.clusterConfig.Nodes) <= 1 {
		slog.Info("ipsec: single-node cluster, skipping enable (no peers)")
		return nil
	}
	configDir := filepath.Dir(d.configPath)
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

	if out, err := sudoCommand("ovs-vsctl", "set", "Open_vSwitch", ".",
		fmt.Sprintf("other_config:certificate=%s", certPath),
		fmt.Sprintf("other_config:private_key=%s", keyPath),
		fmt.Sprintf("other_config:ca_cert=%s", caCertPath),
	).CombinedOutput(); err != nil {
		return fmt.Errorf("set IPsec cert pointers: %s: %w", strings.TrimSpace(string(out)), err)
	}

	if out, err := sudoCommand("ovs-vsctl", "set", "Open_vSwitch", ".",
		"other_config:ipsec_encapsulation=true",
	).CombinedOutput(); err != nil {
		return fmt.Errorf("enable ipsec_encapsulation: %s: %w", strings.TrimSpace(string(out)), err)
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
		out, _ := sudoCommand("systemctl", "is-active", "openvswitch-ipsec.service").CombinedOutput()
		lastOut = strings.TrimSpace(string(out))
		if lastOut == "active" {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("not active after %s: %s (provision via scripts/setup-ovn.sh)", systemctlActiveTimeout, lastOut)
}
