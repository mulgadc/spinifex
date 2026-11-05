//go:build linux

package elbv2

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// arpProbe sends one ARP request for gateway out of iface on an AF_PACKET
// socket bound to it and waits for the gateway's reply on the same socket.
func arpProbe(iface string, mac net.HardwareAddr, src, gateway netip.Addr, wait time.Duration) error {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return fmt.Errorf("interface %s: %w", iface, err)
	}

	proto := htons(syscall.ETH_P_ARP)
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, int(proto))
	if err != nil {
		return fmt.Errorf("packet socket: %w", err)
	}
	defer syscall.Close(fd)

	addr := &syscall.SockaddrLinklayer{Protocol: proto, Ifindex: ifi.Index}
	if err := syscall.Bind(fd, addr); err != nil {
		return fmt.Errorf("bind %s: %w", iface, err)
	}

	deadline := time.Now().Add(wait)
	tv := syscall.NsecToTimeval(int64(wait))
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv); err != nil {
		return fmt.Errorf("set receive deadline: %w", err)
	}

	if err := syscall.Sendto(fd, buildARPRequest(mac, src, gateway), 0, addr); err != nil {
		return fmt.Errorf("send ARP request: %w", err)
	}

	buf := make([]byte, 128)
	for time.Now().Before(deadline) {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) {
				continue
			}
			return fmt.Errorf("receive ARP reply: %w", err)
		}
		if isARPReplyFrom(buf[:n], mac, gateway) {
			return nil
		}
	}
	return errARPNoReply
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }
