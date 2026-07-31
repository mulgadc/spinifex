package host

import (
	"context"
	"strings"
)

// OVNHealth reports the readiness of OVN networking on this compute node.
type OVNHealth struct {
	BrIntExists     bool   `json:"br_int_exists"`
	OVNControllerUp bool   `json:"ovn_controller_up"`
	ChassisID       string `json:"chassis_id,omitempty"`
	EncapIP         string `json:"encap_ip,omitempty"`
	OVNRemote       string `json:"ovn_remote,omitempty"`
}

// HealthStatus probes local OVS/OVN state to determine network readiness.
func HealthStatus() OVNHealth {
	return healthStatus(context.Background(), NewExecRunner())
}

// healthStatus is HealthStatus over an injected Runner. Every probe here is
// read-only and runs unprivileged: ovs-vsctl and ovn-appctl are socket clients
// (utils.NeedsPrivilege), and db.sock plus the ovn-controller ctl socket are
// group-owned by `spinifex`. Escalating instead would mean granting the daemon a
// root-equivalent ovn-appctl rule to read a status.
func healthStatus(ctx context.Context, r Runner) OVNHealth {
	status := OVNHealth{}

	if _, err := r.Run(ctx, "ovs-vsctl", "br-exists", "br-int"); err == nil {
		status.BrIntExists = true
	}

	// Report OVN readiness from the controller's SB connection, not bare process
	// liveness. `ovn-appctl -t ovn-controller` resolves the ctl socket via OVN_RUNDIR
	// regardless of the Trixie (/var/run/ovn) vs older (/var/run/openvswitch) layout;
	// the previous `ovs-appctl -t /var/run/ovn/ovn-controller version` passed a slashed
	// target that ovs-appctl treats as a literal socket path — the real socket is
	// ovn-controller.<pid>.ctl — so it always failed and healthy nodes falsely read
	// not_running. connection-status == "connected" is the meaningful signal: it is
	// true only when the process is up AND synced with the SB RAFT cluster, so a
	// stale-SB wedge correctly surfaces as not_running instead of hiding behind a
	// process-alive check.
	if out, err := r.Run(ctx, "ovn-appctl", "-t", "ovn-controller", "connection-status"); err == nil {
		status.OVNControllerUp = strings.TrimSpace(string(out)) == "connected"
	}

	status.ChassisID = ovsGlobalExternalID(ctx, r, "system-id")
	status.EncapIP = ovsGlobalExternalID(ctx, r, "ovn-encap-ip")
	status.OVNRemote = ovsGlobalExternalID(ctx, r, "ovn-remote")

	return status
}

// ovsGlobalExternalID reads one Open_vSwitch external_ids key, returning "" when
// it is unset or unreadable — the field is omitted from the report rather than
// carrying an error string into it.
func ovsGlobalExternalID(ctx context.Context, r Runner, key string) string {
	out, err := r.Run(ctx, "ovs-vsctl", "get", "Open_vSwitch", ".", "external_ids:"+key)
	if err != nil {
		return ""
	}
	return strings.Trim(strings.TrimSpace(string(out)), "\"")
}
