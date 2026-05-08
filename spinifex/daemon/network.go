package daemon

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// sudoCommand wraps exec.Command with sudo when running as non-root.
// OVS/OVN and ip commands require elevated privileges; in Docker and
// production the daemon runs as root, but in dev environments it may not.
//
// Bound to a var (not a plain func) so tests can swap in a stub — running
// the live binary against the dev host's OVS would mutate `external_ids` on
// the running cluster (see TestSetupComputeNode_ValidatesArgs).
var sudoCommand = func(name string, args ...string) *exec.Cmd {
	if os.Getuid() == 0 {
		return exec.Command(name, args...)
	}
	return exec.Command("sudo", append([]string{name}, args...)...)
}

// OVSNetworkPlumber implements vm.NetworkPlumber using system commands
// (ip, ovs-vsctl); tests use a mock satisfying the same interface.
type OVSNetworkPlumber struct{}

var _ vm.NetworkPlumber = (*OVSNetworkPlumber)(nil)

// SetupTap creates the kernel tap, brings it up, and attaches it to spec.Bridge.
// The pre-create del-port is unconditional because OVS conf.db survives reboot
// while kernel taps don't, so a /sys/class/net check would miss orphan ports.
// Rejected alternative: `--may-exist add-port` — it would silently keep stale
// external_ids from a prior launch with a different ENI/MAC.
func (p *OVSNetworkPlumber) SetupTap(spec vm.TapSpec) error {
	if err := sudoCommand("ovs-vsctl", "--if-exists", "del-port", spec.Bridge, spec.Name).Run(); err != nil {
		slog.Warn("Pre-create del-port failed (continuing)", "tap", spec.Name, "bridge", spec.Bridge, "err", err)
	}
	if _, err := os.Stat("/sys/class/net/" + spec.Name); err == nil {
		if err := sudoCommand("ip", "tuntap", "del", "dev", spec.Name, "mode", "tap").Run(); err != nil {
			slog.Warn("Pre-create tap del failed (continuing)", "tap", spec.Name, "err", err)
		}
	}

	if out, err := sudoCommand("ip", "tuntap", "add", "dev", spec.Name, "mode", "tap").CombinedOutput(); err != nil {
		return fmt.Errorf("create tap %s: %s: %w", spec.Name, strings.TrimSpace(string(out)), err)
	}

	if out, err := sudoCommand("ip", "link", "set", spec.Name, "up").CombinedOutput(); err != nil {
		if cleanErr := sudoCommand("ip", "tuntap", "del", "dev", spec.Name, "mode", "tap").Run(); cleanErr != nil {
			slog.Warn("Failed to clean up tap after bring-up failure", "tap", spec.Name, "err", cleanErr)
		}
		return fmt.Errorf("bring up tap %s: %s: %w", spec.Name, strings.TrimSpace(string(out)), err)
	}

	addPortArgs := []string{"add-port", spec.Bridge, spec.Name}
	if len(spec.ExternalIDs) > 0 {
		addPortArgs = append(addPortArgs, "--", "set", "Interface", spec.Name)
		// Sort keys for deterministic command construction (test assertions, log readability).
		keys := make([]string, 0, len(spec.ExternalIDs))
		for k := range spec.ExternalIDs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			addPortArgs = append(addPortArgs, fmt.Sprintf("external_ids:%s=%s", k, spec.ExternalIDs[k]))
		}
	}
	if out, err := sudoCommand("ovs-vsctl", addPortArgs...).CombinedOutput(); err != nil {
		if cleanErr := sudoCommand("ip", "tuntap", "del", "dev", spec.Name, "mode", "tap").Run(); cleanErr != nil {
			slog.Warn("Failed to clean up tap after OVS failure", "tap", spec.Name, "err", cleanErr)
		}
		return fmt.Errorf("add tap %s to %s: %s: %w", spec.Name, spec.Bridge, strings.TrimSpace(string(out)), err)
	}

	slog.Info("Tap plumbing complete", "tap", spec.Name, "bridge", spec.Bridge, "external_ids", spec.ExternalIDs)
	return nil
}

// CleanupTap removes the named tap from OVS (any bridge) and the kernel.
// Idempotent: callers may invoke for an instance that never reached SetupTap
// (e.g. terminate that races mid-launch).
func (p *OVSNetworkPlumber) CleanupTap(name string) error {
	if out, err := sudoCommand("ovs-vsctl", "--if-exists", "del-port", name).CombinedOutput(); err != nil {
		slog.Warn("Failed to remove tap from OVS", "tap", name, "err", err, "out", strings.TrimSpace(string(out)))
	}

	if _, err := os.Stat("/sys/class/net/" + name); os.IsNotExist(err) {
		slog.Info("Tap already absent", "tap", name)
		return nil
	}

	if out, err := sudoCommand("ip", "tuntap", "del", "dev", name, "mode", "tap").CombinedOutput(); err != nil {
		return fmt.Errorf("delete tap %s: %s: %w", name, strings.TrimSpace(string(out)), err)
	}

	slog.Info("Tap cleanup complete", "tap", name)
	return nil
}

// generateMgmtMAC creates a locally-administered unicast MAC for the
// management NIC. The "mgmt:" tag disambiguates from the dev NIC of the
// same instance (which shares instanceId).
func generateMgmtMAC(instanceId string) string {
	return utils.HashMAC("mgmt:" + instanceId)
}

// GetBridgeIPv4 returns the first IPv4 address on the named Linux bridge.
// Returns "", nil if the bridge does not exist.
func GetBridgeIPv4(bridgeName string) (string, error) {
	iface, err := net.InterfaceByName(bridgeName)
	if err != nil {
		// "no such network interface" means the bridge doesn't exist yet — expected.
		// Other errors (permission denied on /sys/class/net/, etc.) are real failures.
		if strings.Contains(err.Error(), "no such network interface") {
			return "", nil
		}
		return "", fmt.Errorf("lookup %s: %w", bridgeName, err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return "", fmt.Errorf("list addrs on %s: %w", bridgeName, err)
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		if ipNet.IP.To4() != nil {
			return ipNet.IP.String(), nil
		}
	}
	return "", fmt.Errorf("no IPv4 address on %s", bridgeName)
}

// OVNHealthStatus reports the readiness of OVN networking on this compute node.
type OVNHealthStatus struct {
	BrIntExists     bool   `json:"br_int_exists"`
	OVNControllerUp bool   `json:"ovn_controller_up"`
	ChassisID       string `json:"chassis_id,omitempty"`
	EncapIP         string `json:"encap_ip,omitempty"`
	OVNRemote       string `json:"ovn_remote,omitempty"`
}

// CheckOVNHealth probes local OVS/OVN state to determine network readiness.
func CheckOVNHealth() OVNHealthStatus {
	status := OVNHealthStatus{}

	// Check br-int exists
	if err := sudoCommand("ovs-vsctl", "br-exists", "br-int").Run(); err == nil {
		status.BrIntExists = true
	}

	// Check ovn-controller is running via ovs-appctl (more reliable than pgrep)
	if out, err := sudoCommand("ovs-appctl", "-t", "ovn-controller", "version").CombinedOutput(); err == nil && len(out) > 0 {
		status.OVNControllerUp = true
	}

	// Read chassis identity from OVS external_ids
	if out, err := sudoCommand("ovs-vsctl", "get", "Open_vSwitch", ".", "external_ids:system-id").CombinedOutput(); err == nil {
		status.ChassisID = strings.Trim(strings.TrimSpace(string(out)), "\"")
	}
	if out, err := sudoCommand("ovs-vsctl", "get", "Open_vSwitch", ".", "external_ids:ovn-encap-ip").CombinedOutput(); err == nil {
		status.EncapIP = strings.Trim(strings.TrimSpace(string(out)), "\"")
	}
	if out, err := sudoCommand("ovs-vsctl", "get", "Open_vSwitch", ".", "external_ids:ovn-remote").CombinedOutput(); err == nil {
		status.OVNRemote = strings.Trim(strings.TrimSpace(string(out)), "\"")
	}

	return status
}

// SetupComputeNode configures OVS for OVN on this compute node.
// It creates br-int with secure fail-mode and sets the OVN external_ids.
func SetupComputeNode(chassisID, ovnRemote, encapIP string) error {
	// Create br-int if it doesn't exist
	if out, err := sudoCommand("ovs-vsctl", "--may-exist", "add-br", "br-int").CombinedOutput(); err != nil {
		return fmt.Errorf("create br-int: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// Set fail-mode=secure (preserves flows during ovn-controller restart)
	if out, err := sudoCommand("ovs-vsctl", "set", "Bridge", "br-int", "fail-mode=secure").CombinedOutput(); err != nil {
		return fmt.Errorf("set br-int fail-mode: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// Disable in-band management (prevents OVS from adding its own flows)
	if out, err := sudoCommand("ovs-vsctl", "set", "Bridge", "br-int", "other-config:disable-in-band=true").CombinedOutput(); err != nil {
		return fmt.Errorf("set br-int disable-in-band: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// Bring br-int up
	if out, err := sudoCommand("ip", "link", "set", "br-int", "up").CombinedOutput(); err != nil {
		return fmt.Errorf("bring up br-int: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// Set OVN external_ids on the Open_vSwitch table
	if out, err := sudoCommand("ovs-vsctl", "set", "Open_vSwitch", ".",
		fmt.Sprintf("external_ids:system-id=%s", chassisID),
		fmt.Sprintf("external_ids:ovn-remote=%s", ovnRemote),
		fmt.Sprintf("external_ids:ovn-encap-ip=%s", encapIP),
		"external_ids:ovn-encap-type=geneve",
	).CombinedOutput(); err != nil {
		return fmt.Errorf("set OVN external_ids: %s: %w", strings.TrimSpace(string(out)), err)
	}

	slog.Info("OVN compute node configured",
		"chassis_id", chassisID,
		"ovn_remote", ovnRemote,
		"encap_ip", encapIP,
	)

	// Ensure the data NIC is preferred for Geneve tunnel routing.
	if err := EnsureDataRoute(encapIP); err != nil {
		slog.Warn("Failed to configure data NIC routing (Geneve tunnels may use wrong source IP)", "err", err)
	}

	return nil
}

// EnsureDataRoute ensures the kernel routes Geneve tunnel traffic through the
// data NIC (the interface holding the encap IP). When management and data NICs
// share the same subnet with equal route metrics, the kernel may pick the
// management NIC, causing Geneve packets to have the wrong source IP. Remote
// OVS nodes then drop these packets because the source doesn't match the
// expected tunnel remote_ip.
//
// Fix: find the data NIC's subnet route and replace it with a lower metric (50)
// so it's preferred over the management NIC's route (typically metric 100+).
func EnsureDataRoute(encapIP string) error {
	dataIface, err := findInterfaceByIP(encapIP)
	if err != nil {
		return fmt.Errorf("find data interface for %s: %w", encapIP, err)
	}

	// Read the existing subnet route for the data interface
	out, err := sudoCommand("ip", "-o", "-4", "route", "show", "dev", dataIface, "proto", "kernel", "scope", "link").CombinedOutput()
	if err != nil {
		return fmt.Errorf("read routes for %s: %s: %w", dataIface, strings.TrimSpace(string(out)), err)
	}

	// Parse the subnet CIDR from the route output (e.g. "10.1.0.0/16 dev eth1 ...")
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 {
		return fmt.Errorf("no kernel route found for %s", dataIface)
	}
	subnet := fields[0]

	// Replace the route with a lower metric so the data NIC is preferred
	if out, err := sudoCommand("ip", "route", "replace", subnet,
		"dev", dataIface, "src", encapIP, "metric", "50",
	).CombinedOutput(); err != nil {
		return fmt.Errorf("replace route for %s: %s: %w", subnet, strings.TrimSpace(string(out)), err)
	}

	slog.Info("Data NIC route configured for Geneve tunnels",
		"interface", dataIface,
		"subnet", subnet,
		"encap_ip", encapIP,
		"metric", 50,
	)
	return nil
}

// findInterfaceByIP returns the network interface name that holds the given IP address.
func findInterfaceByIP(ipAddr string) (string, error) {
	targetIP := net.ParseIP(ipAddr)
	if targetIP == nil {
		return "", fmt.Errorf("invalid IP address: %s", ipAddr)
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		return "", fmt.Errorf("list interfaces: %w", err)
	}

	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			if ipNet.IP.Equal(targetIP) {
				return iface.Name, nil
			}
		}
	}
	return "", fmt.Errorf("no interface found with IP %s", ipAddr)
}
