package host

import (
	"fmt"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/utils"
)

// SetIPSecCertPaths writes the local IPsec peer cert pointers into the OVS
// Open_vSwitch table. ovs-monitor-ipsec reads these to materialise strongSwan
// configs for every Geneve tunnel ovn-controller programs.
func SetIPSecCertPaths(certPath, keyPath, caCertPath string) error {
	out, err := utils.SudoCommand("ovs-vsctl", "set", "Open_vSwitch", ".",
		fmt.Sprintf("other_config:certificate=%s", certPath),
		fmt.Sprintf("other_config:private_key=%s", keyPath),
		fmt.Sprintf("other_config:ca_cert=%s", caCertPath),
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("set IPsec cert pointers: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// EnableIPSecEncapsulation flips ipsec_encapsulation=true on the local
// Open_vSwitch row. Caller must first verify ovs-monitor-ipsec is active —
// flipping without a live daemon creates the silent-drop trap (mulga-siv-136).
func EnableIPSecEncapsulation() error {
	out, err := utils.SudoCommand("ovs-vsctl", "set", "Open_vSwitch", ".",
		"other_config:ipsec_encapsulation=true",
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("enable ipsec_encapsulation: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// SetNBGlobalIPSec writes NB_Global.ipsec on the local OVN NB DB. Triggers
// ovn-controller on every chassis to add options:remote_name to Geneve tunnel
// ports, which is what ovs-monitor-ipsec keys off to materialise strongSwan
// connections. Only the management node has a local NB socket; callers gate
// the call on socket presence.
func SetNBGlobalIPSec(enable bool) error {
	val := "false"
	if enable {
		val = "true"
	}
	out, err := utils.SudoCommand("ovn-nbctl", "set", "NB_Global", ".", "ipsec="+val).CombinedOutput()
	if err != nil {
		return fmt.Errorf("set NB_Global ipsec=%s: %s: %w", val, strings.TrimSpace(string(out)), err)
	}
	return nil
}
