package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/admin"
)

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
func (d *Daemon) enableOVNIPSec() error {
	if d.configPath == "" {
		return fmt.Errorf("config path unset")
	}
	configDir := filepath.Dir(d.configPath)
	certPath, keyPath := admin.IPSecCertPaths(configDir)
	caCertPath := filepath.Join(configDir, "ca.pem")

	for _, p := range []string{certPath, keyPath, caCertPath} {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("missing IPsec credential %s: %w", p, err)
		}
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
