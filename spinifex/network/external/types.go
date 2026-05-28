package external

// ExternalPoolConfig describes one external IP pool wired to a VPC.
// Mirrors [external_pools] in spinifex.toml.
type ExternalPoolConfig struct {
	Name string
	// Source selects the IP source: "static" (default) for inline range
	// math or "dhcp" for RFC 2131 DORA via vpcd's DHCPManager.
	Source string
	// BindBridge is the Linux bridge the DHCP client runs against
	// (required when Source="dhcp"). Empty for static pools.
	BindBridge      string
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

// IsDHCP reports whether the pool sources IPs from an upstream DHCP server.
func (p *ExternalPoolConfig) IsDHCP() bool {
	return p != nil && p.Source == SourceDHCP
}

const (
	// SourceStatic is the default pool source (inline range math, KV-backed).
	SourceStatic = "static"
	// SourceDHCP delegates allocation to vpcd's DHCPManager.
	SourceDHCP = "dhcp"
)

// IGWSpec is the L5 input for IGWManager.AttachIGW / DetachIGW.
// InternetGatewayID is propagated into OVN external_ids for reconcile correlation.
type IGWSpec struct {
	VPCID             string
	InternetGatewayID string
}
