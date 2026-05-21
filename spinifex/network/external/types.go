package external

// ExternalPoolConfig describes one external IP pool wired to a VPC. Mirrors
// the [external_pools] entry in spinifex.toml. Two pool kinds are supported:
//
//   - Source="static": IPs come from RangeStart..RangeEnd, gateway from
//     Gateway, and the gateway LRP IP from GwLrpRangeStart..GwLrpRangeEnd
//     (auto-derived from the WAN subnet when unset).
//
//   - Source="dhcp": IPs come from upstream router DHCP. The gateway LRP IP
//     is acquired via upstream DHCP using the pool's DhcpBindBridge. This
//     package does not include the DHCP client; callers inject a
//     GatewayIPAllocator that knows how to talk to the DHCP store.
//
// This type duplicates services/vpcd/vpcd.go's ExternalPoolConfig. Dedup is
// out of scope for this bead; the two will merge when L2 (topology) grows
// the L5-driving methods and vpcd's local copy disappears.
type ExternalPoolConfig struct {
	Name            string
	Source          string
	RangeStart      string
	RangeEnd        string
	Gateway         string
	GatewayIP       string
	PrefixLen       int
	DNSServers      []string
	Region          string
	AZ              string
	DhcpBindBridge  string
	GwLrpRangeStart string
	GwLrpRangeEnd   string
}

// IsDHCP reports whether the pool acquires IPs from an upstream DHCP server.
func (p *ExternalPoolConfig) IsDHCP() bool {
	return p.Source == "dhcp"
}

// IGWSpec is the L5 input for IGWManager.AttachIGW / DetachIGW. VPCID is
// the only required field; InternetGatewayID is propagated into OVN
// external_ids so reconcile paths can correlate state.
type IGWSpec struct {
	VPCID             string
	InternetGatewayID string
}
