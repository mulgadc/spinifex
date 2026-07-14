package host

import (
	"strings"

	"github.com/mulgadc/spinifex/spinifex/utils"
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
	status := OVNHealth{}

	if err := utils.SudoCommand("ovs-vsctl", "br-exists", "br-int").Run(); err == nil {
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
	if out, err := utils.SudoCommand("ovn-appctl", "-t", "ovn-controller", "connection-status").Output(); err == nil {
		status.OVNControllerUp = strings.TrimSpace(string(out)) == "connected"
	}

	if out, err := utils.SudoCommand("ovs-vsctl", "get", "Open_vSwitch", ".", "external_ids:system-id").CombinedOutput(); err == nil {
		status.ChassisID = strings.Trim(strings.TrimSpace(string(out)), "\"")
	}
	if out, err := utils.SudoCommand("ovs-vsctl", "get", "Open_vSwitch", ".", "external_ids:ovn-encap-ip").CombinedOutput(); err == nil {
		status.EncapIP = strings.Trim(strings.TrimSpace(string(out)), "\"")
	}
	if out, err := utils.SudoCommand("ovs-vsctl", "get", "Open_vSwitch", ".", "external_ids:ovn-remote").CombinedOutput(); err == nil {
		status.OVNRemote = strings.Trim(strings.TrimSpace(string(out)), "\"")
	}

	return status
}
