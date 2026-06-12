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
		"mtu":        "1442",
	}
}
