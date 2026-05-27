package host

import (
	"os"
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

	// Trixie OVN moved the pid file to /var/run/ovn/; older installs use
	// /var/run/openvswitch/. Probe at runtime so the same binary works on both.
	ovnTarget := "ovn-controller"
	if _, err := os.Stat("/var/run/ovn/ovn-controller.pid"); err == nil {
		ovnTarget = "/var/run/ovn/ovn-controller"
	}
	if out, err := utils.SudoCommand("ovs-appctl", "-t", ovnTarget, "version").CombinedOutput(); err == nil && len(out) > 0 {
		status.OVNControllerUp = true
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
