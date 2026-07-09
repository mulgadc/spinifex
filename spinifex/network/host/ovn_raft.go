package host

import (
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/utils"
)

// OVN DB ctl targets and schemas for cluster/status probes. The short "-t"
// target lets ovn-appctl resolve the ctl socket via OVN_RUNDIR regardless of
// the Trixie (/var/run/ovn) vs older (/var/run/openvswitch) layout.
const (
	OVNNBTarget = "ovnnb_db"
	OVNSBTarget = "ovnsb_db"
	OVNNBSchema = "OVN_Northbound"
	OVNSBSchema = "OVN_Southbound"
)

// ovnAppctlTimeout bounds the cluster/status shell-out so a wedged ovsdb-server
// cannot stall the node-status handler, matching the 500ms budget of the
// sibling NATS/Predastore role probes.
const ovnAppctlTimeout = 500 * time.Millisecond

// OVNDBRole reports this node's OVSDB Raft role for the given ctl target and
// schema: "leader", "follower", or "" when OVN is standalone or unreachable.
// Callers gate this on quorum membership, so an appctl error means a DB that
// should be reachable is not — logged at Warn rather than silently dropped.
func OVNDBRole(target, schema string) string {
	ctx, cancel := context.WithTimeout(context.Background(), ovnAppctlTimeout)
	defer cancel()
	out, err := utils.SudoCommandContext(ctx, "ovn-appctl", "-t", target, "cluster/status", schema).Output()
	if err != nil {
		slog.Warn("Failed to query OVN cluster/status on a quorum member",
			"schema", schema, "target", target, "err", err, "stderr", commandStderr(err))
		return ""
	}
	_, role := ParseClusterStatus(string(out))
	return role
}

// commandStderr returns the subprocess stderr captured by exec.Cmd.Output for an
// ExitError; err.Error() alone reports only the exit code.
func commandStderr(err error) string {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return strings.TrimSpace(string(exitErr.Stderr))
	}
	return ""
}

// ParseClusterStatus extracts the quorum size and this server's role from ovs
// cluster/status output. Each member appears as a "<id> at tcp:<addr>" line
// under Servers:; the "Role:" header reports the queried server's own role.
func ParseClusterStatus(out string) (servers int, role string) {
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "Role:"); ok {
			role = strings.TrimSpace(rest)
		}
		if strings.Contains(line, " at tcp:") {
			servers++
		}
	}
	return servers, role
}
