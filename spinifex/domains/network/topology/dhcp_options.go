package topology

import "strconv"

// MTU budget for the overlay, subtracted from the underlay figure.
//
// geneveOverhead is outer IPv4 20 + UDP 8 + Geneve base 8 + OVN's TLV metadata
// option 8 + inner Ethernet 14. espOverhead is transport-mode ESP with
// rfc4106(gcm(aes)): SPI, sequence, IV, pad and ICV.
//
// Both assume an IPv4 underlay. IPv6 tunnel endpoints cost a further 20, and
// Geneve options are variable in principle, so treat these as exact for the
// encapsulation we actually emit rather than as a universal ceiling.
const (
	geneveOverhead = 58
	espOverhead    = 34

	// DefaultUnderlayMTU is the standard Ethernet payload. Deliberately not
	// probed from the local NIC: a node's own MTU says nothing about the switch
	// between it and its peers, and guessing high blackholes large frames with
	// no "fragmentation needed" coming back from an L2 fabric.
	DefaultUnderlayMTU = 1500

	// MinUnderlayMTU is the smallest underlay that leaves a guest the IPv4
	// minimum reassembly buffer of 576 once encapsulation is paid for.
	MinUnderlayMTU = 576 + geneveOverhead + espOverhead
)

// SubnetMTU returns the guest MTU to advertise over DHCP for a given underlay.
//
// The overhead is passed on to the guest rather than hidden from it. Hiding it
// means running the fabric far enough above 1500 that a standard guest MTU fits
// inside the headroom, which is what the large public clouds do — but they own
// the switches. On a fabric we do not control, the honest figure is the one the
// path can actually carry, because the failure mode of overstating it is a
// silent blackhole of large segments, not a slowdown.
//
// A jumbo underlay therefore reaches the guest: 9000 gives 8942, or 8908 with
// IPsec. Raising the underlay is the lever; this function just spends it.
func SubnetMTU(underlayMTU int, ipsecEnabled bool) int {
	if underlayMTU < MinUnderlayMTU {
		underlayMTU = DefaultUnderlayMTU
	}
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
func BuildSubnetDHCPOptions(gwIP, routerMAC, dnsServer string, underlayMTU int, ipsecEnabled bool) map[string]string {
	return map[string]string{
		"server_id":  gwIP,
		"server_mac": routerMAC,
		"lease_time": "3600",
		"router":     gwIP,
		"dns_server": dnsServer,
		"mtu":        strconv.Itoa(SubnetMTU(underlayMTU, ipsecEnabled)),
	}
}
