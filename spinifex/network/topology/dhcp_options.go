package topology

// BuildSubnetDHCPOptions returns the OVN DHCPOptions map for a subnet. Shared
// by the live manager and reconciler to prevent dns_server drift. IMDS is not
// steered via option 121; the subnet-switch localport answers L2 ARP directly.
func BuildSubnetDHCPOptions(gwIP, routerMAC, dnsServer string) map[string]string {
	return map[string]string{
		"server_id":  gwIP,
		"server_mac": routerMAC,
		"lease_time": "3600",
		"router":     gwIP,
		"dns_server": dnsServer,
		// 1442 is the geneve-only figure (1500 - 58) and overstates the usable
		// MTU wherever the egress path adds encapsulation: on an IPsec-protected
		// NAT path the measured path MTU is 1408, so a guest that believes 1442
		// advertises an MSS ~34B too large. Small packets pass; large inbound
		// segments (a TLS ServerHello + certificate chain, image layers) are
		// silently dropped, which surfaces as `TLS handshake timeout` rather than
		// as anything MTU-shaped. Advertise the conservative figure — a few bytes
		// of payload per segment is cheaper than a PMTU blackhole, and PMTU
		// discovery still raises the effective size per destination where the
		// path allows it.
		"mtu": "1408",
	}
}
