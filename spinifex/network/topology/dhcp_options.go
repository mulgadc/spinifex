package topology

import "strconv"

// Overlay MTU budget, from the 1500-byte underlay. Geneve costs 58 bytes on
// every packet; ESP transport mode with rfc4106(gcm(aes)) costs a further 34.
const (
	underlayMTU    = 1500
	geneveOverhead = 58
	espOverhead    = 34
)

// SubnetMTU returns the guest MTU to advertise over DHCP. Overstating it is the
// expensive mistake: small packets pass, large inbound segments (a TLS
// ServerHello and certificate chain, image layers) are silently dropped, and it
// surfaces as `TLS handshake timeout` rather than as anything MTU-shaped.
//
// The conservative figure is therefore the one that survives the worst egress
// path. With IPsec off there is no ESP header to budget for and 1442 is exact,
// worth roughly 10% of throughput over 1408.
func SubnetMTU(ipsecEnabled bool) int {
	mtu := underlayMTU - geneveOverhead
	if ipsecEnabled {
		mtu -= espOverhead
	}
	return mtu
}

// BuildSubnetDHCPOptions returns the OVN DHCPOptions map for a subnet. Shared
// by the live manager and reconciler to prevent dns_server drift. IMDS is not
// steered via option 121: a guest either routes to it via the gateway, where the
// br-imds ingress demux catches it, or resolves it on-link, where the per-tap ARP
// responder answers. Both live in network/host/imds_datapath.go.
func BuildSubnetDHCPOptions(gwIP, routerMAC, dnsServer string, ipsecEnabled bool) map[string]string {
	return map[string]string{
		"server_id":  gwIP,
		"server_mac": routerMAC,
		"lease_time": "3600",
		"router":     gwIP,
		"dns_server": dnsServer,
		"mtu":        strconv.Itoa(SubnetMTU(ipsecEnabled)),
	}
}
