package host

import (
	"bytes"
	"errors"
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
// flipping without a live daemon creates a silent-drop trap.
func EnableIPSecEncapsulation() error {
	out, err := utils.SudoCommand("ovs-vsctl", "set", "Open_vSwitch", ".",
		"other_config:ipsec_encapsulation=true",
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("enable ipsec_encapsulation: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// errNBUnreachable marks the one failure that means "this node has no local NB
// DB" rather than "the read broke". A permission change on the socket, a missing
// binary or a timed-out transaction are all real faults on a management node,
// and lumping them in with this would silence them.
var errNBUnreachable = errors.New("no local OVN NB DB")

// nbUnreachablePatterns are what ovsdb's client library prints when it cannot
// open or complete a handshake on the socket.
var nbUnreachablePatterns = []string{
	"database connection failed",
	"connection refused",
	"no such file or directory",
}

// GetNBGlobalIPSec reads NB_Global.ipsec from the local OVN NB DB. The error
// doubles as the reachability answer: a present socket file says nothing about
// whether the database behind it accepts connections yet.
//
// Reads stdout alone. ovn-nbctl writes vlog lines to stderr on a successful run,
// and folding those into the value parses a live "true" as false.
func GetNBGlobalIPSec() (bool, error) {
	cmd := utils.SudoCommand("ovn-nbctl", "--timeout=5", "get", "NB_Global", ".", "ipsec")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		wrapped := fmt.Errorf("get NB_Global ipsec: %s: %w", msg, err)
		if isNBUnreachable(msg) {
			return false, fmt.Errorf("%w: %w", errNBUnreachable, wrapped)
		}
		return false, wrapped
	}
	return strings.Trim(strings.TrimSpace(string(out)), `"`) == "true", nil
}

func isNBUnreachable(msg string) bool {
	lower := strings.ToLower(msg)
	for _, pattern := range nbUnreachablePatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

// SetNBGlobalIPSec writes NB_Global.ipsec on the local OVN NB DB, triggering
// ovn-controller to add options:remote_name to Geneve tunnels for strongSwan.
// Only the management node has a reachable NB DB; callers gate on that.
func SetNBGlobalIPSec(enable bool) error {
	val := "false"
	if enable {
		val = "true"
	}
	out, err := utils.SudoCommand("ovn-nbctl", "--timeout=5", "set", "NB_Global", ".", "ipsec="+val).CombinedOutput()
	if err != nil {
		return fmt.Errorf("set NB_Global ipsec=%s: %s: %w", val, strings.TrimSpace(string(out)), err)
	}
	return nil
}
