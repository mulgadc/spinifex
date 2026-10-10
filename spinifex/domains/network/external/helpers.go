package external

import (
	"encoding/binary"
	"net"
)

func ipv4ToUint32(ip net.IP) uint32 {
	return binary.BigEndian.Uint32(ip.To4())
}

func uint32ToIPv4(n uint32) net.IP {
	return net.IPv4(byte(n>>24&0xff), byte(n>>16&0xff), byte(n>>8&0xff), byte(n&0xff)).To4()
}
