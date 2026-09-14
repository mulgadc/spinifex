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

// errNBUnreachable marks the one failure that means "no NB DB answered" rather
// than "the read broke". A permission change on the socket, a missing binary or
// a timed-out transaction are real faults, and lumping them in would silence them.
var errNBUnreachable = errors.New("no OVN NB DB reachable")

// nbUnreachablePatterns are what ovsdb's client library prints when it cannot
// open or complete a handshake with any of its remotes.
var nbUnreachablePatterns = []string{
	"database connection failed",
	"connection refused",
	"no such file or directory",
}

// nbctlArgs targets nbAddr (the cluster remote list; empty is the local socket).
// Reads accept any connected member, so a raft follower answers instead of
// refusing; writes stay leader-only so ovn-nbctl walks the remotes to the leader.
func nbctlArgs(nbAddr string, leaderOnly bool, cmd ...string) []string {
	var args []string
	if nbAddr != "" {
		args = append(args, "--db="+nbAddr)
	}
	if !leaderOnly {
		args = append(args, "--no-leader-only")
	}
	return append(append(args, "--timeout=5"), cmd...)
}

// GetNBGlobalIPSec reads NB_Global.ipsec. The error doubles as the reachability
// answer: a present socket file says nothing about whether the database behind
// it accepts connections yet.
//
// Reads stdout alone. ovn-nbctl writes vlog lines to stderr on a successful run,
// and folding those into the value parses a live "true" as false.
func GetNBGlobalIPSec(nbAddr string) (bool, error) {
	cmd := utils.SudoCommand("ovn-nbctl", nbctlArgs(nbAddr, false, "get", "NB_Global", ".", "ipsec")...)
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

// SetNBGlobalIPSec writes NB_Global.ipsec, triggering ovn-controller to add
// options:remote_name to Geneve tunnels for strongSwan. Callers elect a single
// writer, so this never races another node's write.
func SetNBGlobalIPSec(nbAddr string, enable bool) error {
	val := "false"
	if enable {
		val = "true"
	}
	out, err := utils.SudoCommand("ovn-nbctl", nbctlArgs(nbAddr, true, "set", "NB_Global", ".", "ipsec="+val)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("set NB_Global ipsec=%s: %s: %w", val, strings.TrimSpace(string(out)), err)
	}
	return nil
}
