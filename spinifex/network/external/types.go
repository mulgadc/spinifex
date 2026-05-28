package external

// ExternalPoolConfig describes one external IP pool wired to a VPC.
// Mirrors [external_pools] in spinifex.toml.
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

// IGWSpec is the L5 input for IGWManager.AttachIGW / DetachIGW.
// InternetGatewayID is propagated into OVN external_ids for reconcile correlation.
type IGWSpec struct {
	VPCID             string
	InternetGatewayID string
}
