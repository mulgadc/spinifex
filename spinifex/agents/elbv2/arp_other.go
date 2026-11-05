//go:build !linux

package elbv2

import (
	"errors"
	"net"
	"net/netip"
	"time"
)

// arpProbe is only meaningful inside the Linux guest.
func arpProbe(string, net.HardwareAddr, netip.Addr, netip.Addr, time.Duration) error {
	return errors.New("ARP probe is only supported on linux")
}
