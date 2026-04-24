package admin

import (
	"fmt"
	"net"
	"os/exec"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/utils"
)

// DetectedInterface represents a network interface discovered on the host.
type DetectedInterface struct {
	Name      string // e.g., "enp0s3"
	IP        string // e.g., "192.168.1.21"
	Subnet    string // e.g., "192.168.0.0/23"
	PrefixLen int    // e.g., 23
	Gateway   string // non-empty only for WAN (default route)
	Role      string // "wan", "lan"
}

// DetectedNetwork holds the auto-detected network topology.
type DetectedNetwork struct {
	Interfaces []DetectedInterface
	WAN        *DetectedInterface // The interface with the default route
	LANCount   int                // Number of non-WAN interfaces
}

// DetectNetwork auto-detects the host's network topology from routing table.
// It identifies the WAN interface (default route) and LAN interfaces.
func DetectNetwork() (*DetectedNetwork, error) {
	// Get default route: "default via 192.168.1.1 dev enp0s3 ..."
	out, err := exec.Command("ip", "-4", "route", "show", "default").Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get default route: %w", err)
	}
	defaultRoute := strings.TrimSpace(string(out))
	if defaultRoute == "" {
		return nil, fmt.Errorf("no default route found — host has no internet connectivity")
	}

	// Parse: "default via 192.168.1.1 dev enp0s3 ..."
	var wanGateway, wanIface string
	fields := strings.Fields(defaultRoute)
	for i, f := range fields {
		if f == "via" && i+1 < len(fields) {
			wanGateway = fields[i+1]
		}
		if f == "dev" && i+1 < len(fields) {
			wanIface = fields[i+1]
		}
	}
	if wanIface == "" || wanGateway == "" {
		return nil, fmt.Errorf("could not parse default route: %s", defaultRoute)
	}

	// Get all routes to find interfaces and their subnets
	allRoutes, err := exec.Command("ip", "-4", "route", "show", "scope", "link").Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get routes: %w", err)
	}

	// Parse routes: "192.168.0.0/23 dev enp0s3 ... src 192.168.1.21"
	seen := make(map[string]bool)
	var interfaces []DetectedInterface

	for line := range strings.SplitSeq(strings.TrimSpace(string(allRoutes)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}

		subnet := fields[0]
		var ifaceName, srcIP string
		for i, f := range fields {
			if f == "dev" && i+1 < len(fields) {
				ifaceName = fields[i+1]
			}
			if f == "src" && i+1 < len(fields) {
				srcIP = fields[i+1]
			}
		}
		if ifaceName == "" || srcIP == "" {
			continue
		}

		// Skip virtual/docker/ovs interfaces (but allow the default route device)
		if isVirtualInterface(ifaceName, wanIface) {
			continue
		}

		// Skip duplicates (same interface may have multiple routes)
		if seen[ifaceName] {
			continue
		}
		seen[ifaceName] = true

		prefixLen := 24 // default
		if _, ipNet, err := net.ParseCIDR(subnet); err == nil {
			ones, _ := ipNet.Mask.Size()
			prefixLen = ones
		}

		iface := DetectedInterface{
			Name:      ifaceName,
			IP:        srcIP,
			Subnet:    subnet,
			PrefixLen: prefixLen,
		}

		if ifaceName == wanIface {
			iface.Gateway = wanGateway
			iface.Role = "wan"
		} else {
			iface.Role = "lan"
		}

		interfaces = append(interfaces, iface)
	}

	if len(interfaces) == 0 {
		return nil, fmt.Errorf("no usable network interfaces found")
	}

	result := &DetectedNetwork{
		Interfaces: interfaces,
	}

	for i := range interfaces {
		if interfaces[i].Role == "wan" {
			result.WAN = &interfaces[i]
		} else {
			result.LANCount++
		}
	}

	return result, nil
}

// SuggestPoolRange suggests a default external IP pool range based on the
// WAN subnet. Places the pool at the high end of the subnet to avoid
// collisions with typical DHCP ranges (which start low).
func SuggestPoolRange(wan *DetectedInterface) (start, end string) {
	_, ipNet, err := net.ParseCIDR(wan.Subnet)
	if err != nil {
		return "", ""
	}

	// Calculate broadcast address
	ip := ipNet.IP.To4()
	mask := ipNet.Mask
	broadcast := make(net.IP, 4)
	for i := range ip {
		broadcast[i] = ip[i] | ^mask[i]
	}

	// Pool: broadcast - 100 to broadcast - 5
	// e.g., for 192.168.0.0/23 (broadcast 192.168.1.255):
	//   start = 192.168.1.155, end = 192.168.1.250
	endIP := make(net.IP, 4)
	copy(endIP, broadcast)
	endIP[3] -= 5 // Leave some room at top

	startIP := make(net.IP, 4)
	copy(startIP, endIP)
	// Go back 100 IPs
	carry := 100
	for i := 3; i >= 0 && carry > 0; i-- {
		val := int(startIP[i]) - carry
		if val < 0 {
			startIP[i] = utils.SafeIntToUint8(256 + val)
			carry = 1
		} else {
			startIP[i] = utils.SafeIntToUint8(val)
			carry = 0
		}
	}

	return startIP.String(), endIP.String()
}

// isVirtualInterface returns true for interfaces that shouldn't be
// considered as physical NICs. The defaultRouteDev parameter is the
// interface carrying the default route — it's always considered physical
// even if its name matches a virtual prefix (e.g., br-wan).
func isVirtualInterface(name string, defaultRouteDev string) bool {
	// The default route interface is always considered physical — it's the
	// WAN uplink, even if it's a bridge (br-wan, br-external, etc.).
	if defaultRouteDev != "" && name == defaultRouteDev {
		return false
	}
	prefixes := []string{
		"docker", "br-", "veth", "ovs", "virbr",
		"tun", "tap", "tailscale", "wg", "lo",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
