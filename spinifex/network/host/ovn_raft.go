package host

import (
	"context"
	"log/slog"
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
// A standalone (non-clustered) DB or any appctl error fails soft to "" at Debug,
// matching the NATS/Predastore role probes.
func OVNDBRole(target, schema string) string {
	ctx, cancel := context.WithTimeout(context.Background(), ovnAppctlTimeout)
	defer cancel()
	out, err := utils.SudoCommandContext(ctx, "ovn-appctl", "-t", target, "cluster/status", schema).Output()
	if err != nil {
		slog.Debug("Failed to query OVN cluster/status", "schema", schema, "err", err)
		return ""
	}
	_, role := ParseClusterStatus(string(out))
	return role
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
