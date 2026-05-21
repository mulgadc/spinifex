package external

import (
	"net"

	"github.com/mulgadc/spinifex/spinifex/utils"
)

// generateMAC builds a deterministic locally-administered unicast MAC from
// a resource ID. The same algorithm vpcd has used since the L1/L2 split —
// callers within this package use it for gateway-LRP MAC assignment.
func generateMAC(resourceID string) string {
	return utils.HashMAC(resourceID)
}

func ipv4ToUint32(ip net.IP) uint32 {
	v := ip.To4()
	return uint32(v[0])<<24 | uint32(v[1])<<16 | uint32(v[2])<<8 | uint32(v[3])
}

func uint32ToIPv4(n uint32) net.IP {
	return net.IPv4(byte(n>>24&0xff), byte(n>>16&0xff), byte(n>>8&0xff), byte(n&0xff)).To4()
}
