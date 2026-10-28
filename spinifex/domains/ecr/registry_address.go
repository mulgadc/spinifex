package ecr

import "net"

// RegistryAddress returns the registry host and port a gateway node advertises.
// A concrete AWSGW bind host wins, then a concrete AdvertiseIP; an empty host
// means the per-account parity name applies. The port comes from the bind.
func RegistryAddress(awsgwHost, advertiseIP string) (host, port string) {
	if h, p, err := net.SplitHostPort(awsgwHost); err == nil {
		port = p
		if isConcreteRegistryHost(h) {
			host = h
		}
	}
	if host == "" && isConcreteRegistryHost(advertiseIP) {
		host = advertiseIP
	}
	return host, port
}

// isConcreteRegistryHost reports whether host is a non-empty, non-wildcard
// literal, so a registry URI never carries 0.0.0.0 or ::.
func isConcreteRegistryHost(host string) bool {
	return host != "" && host != "0.0.0.0" && host != "::"
}
