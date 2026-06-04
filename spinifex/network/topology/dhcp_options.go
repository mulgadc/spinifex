package topology

// BuildSubnetDHCPOptions returns the OVN DHCPOptions Options map emitted for a
// subnet's DHCPOptions row. Both the live topology manager and the reconciler
// call it so the two paths cannot drift — they previously hard-coded divergent
// dns_server values (live used the configured server, the reconciler "8.8.8.8").
//
// IMDS reachability (169.254.169.254) is not steered via DHCP option 121: the
// default gateway stays option 3 (router), and the localport on the subnet
// switch answers ARP for the metadata IP directly (one L2 hop, no router). The
// guest's on-link route to 169.254.169.254 is delivered out-of-band by the
// cloud-init network-config the instance handler renders (NoCloud seed) — see
// generateNetworkConfig in handlers/ec2/instance — so it reaches DHCP and static
// guests alike without folding the default route into option 121 (RFC 3442).
func BuildSubnetDHCPOptions(gwIP, routerMAC, dnsServer string) map[string]string {
	return map[string]string{
		"server_id":  gwIP,
		"server_mac": routerMAC,
		"lease_time": "3600",
		"router":     gwIP,
		"dns_server": dnsServer,
		"mtu":        "1442",
	}
}
