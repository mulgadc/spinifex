// Package http provides generic HTTP ingress helpers.
package http

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// RequestClientIP is the caller's address: X-Real-IP when the connection is
// from loopback, where only a local proxy (the nginx edge or console) can have
// set it, and RemoteAddr otherwise, so a direct client cannot choose what is
// logged.
func RequestClientIP(r *http.Request) string {
	ip := ClientIP(r.RemoteAddr)
	if addr, err := netip.ParseAddr(ip); err != nil || !addr.IsLoopback() {
		return ip
	}
	realIP, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("X-Real-IP")))
	if err != nil {
		return ip
	}
	return realIP.String()
}

// ClientIP returns the IP from a RemoteAddr, stripping the port. Handles both
// IPv4 and IPv6, and tolerates a RemoteAddr that is already a bare IP (no port).
func ClientIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}
