package external

// ExternalPoolConfig describes one external IP pool wired to a VPC. Mirrors
// the [external_pools] entry in spinifex.toml. IPs come from
// RangeStart..RangeEnd, the gateway from Gateway, and the gateway LRP IP
// from GwLrpRangeStart..GwLrpRangeEnd (auto-derived from the WAN subnet
// when unset). The upstream-DHCP source model is gone (mulga-siv-125.3.3).
//
// This type duplicates services/vpcd/vpcd.go's ExternalPoolConfig. Dedup is
// out of scope for this bead; the two will merge when L2 (topology) grows
// the L5-driving methods and vpcd's local copy disappears.
type ExternalPoolConfig struct {
	Name            string
	RangeStart      string
	RangeEnd        string
	Gateway         string
	GatewayIP       string
	PrefixLen       int
	DNSServers      []string
	Region          string
	AZ              string
	GwLrpRangeStart string
	GwLrpRangeEnd   string
}

// IGWSpec is the L5 input for IGWManager.AttachIGW / DetachIGW. VPCID is
// the only required field; InternetGatewayID is propagated into OVN
// external_ids so reconcile paths can correlate state.
type IGWSpec struct {
	VPCID             string
	InternetGatewayID string
}
