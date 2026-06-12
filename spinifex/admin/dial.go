package admin

import "net"

// DialTarget returns a host:port for in-process dialing. Wildcard listeners
// ("0.0.0.0:N", "[::]:N") are rewritten to loopback; specific IPs pass through.
func DialTarget(listenAddr string) string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return listenAddr
	}
	switch host {
	case "0.0.0.0", "":
		return net.JoinHostPort("127.0.0.1", port)
	case "::":
		return net.JoinHostPort("::1", port)
	}
	return listenAddr
}

// AdvertiseHost returns the host off-host clients should dial. Wildcard listeners
// return advertiseIP; specific listen hosts return themselves (port is dropped).
func AdvertiseHost(listenAddr, advertiseIP string) string {
	host, _, err := net.SplitHostPort(listenAddr)
	if err != nil {
		host = listenAddr
	}
	switch host {
	case "0.0.0.0", "::", "":
		return advertiseIP
	}
	return host
}
