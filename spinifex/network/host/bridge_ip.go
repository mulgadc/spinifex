package host

import (
	"fmt"
	"net"
	"strings"
)

// GetBridgeIPv4 returns the first IPv4 address on the named Linux bridge.
// Returns "", nil if the bridge does not exist.
func GetBridgeIPv4(bridgeName string) (string, error) {
	iface, err := net.InterfaceByName(bridgeName)
	if err != nil {
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
