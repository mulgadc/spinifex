package topology

// BuildSubnetDHCPOptions returns the OVN DHCPOptions Options map emitted for a
// subnet's DHCPOptions row. Both the live topology manager and the reconciler
// call it so the two paths cannot drift — they previously hard-coded divergent
// dns_server values (live used the configured server, the reconciler "8.8.8.8").
//
// IMDS reachability (169.254.169.254) is no longer steered via DHCP option 121; it
// is answered link-local by the subnet router LSP's options:arp_proxy, so DHCP and
// static guests reach IMDS identically. The default gateway stays option 3 (router).
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
