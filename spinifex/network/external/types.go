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
	BindBridge string
	// DHCPMAC picks the DHCP client MAC strategy: "derived" (default,
	// per-client-id 02:xx MACs) or "interface" (the bind interface's own
	// MAC, for uplinks that drop foreign source MACs — WiFi/WWAN).
	DHCPMAC         string
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
	// OCICompartmentID, OCIVNICID and OCIVNICIface identify where the provider
	// creates addresses (Source="oci"). Exactly one of the VNIC fields is set;
	// the interface name is resolved to an OCID against IMDS at startup.
	OCICompartmentID string
	OCIVNICID        string
	OCIVNICIface     string
	OCISubnetID      string
	OCIPublicIPPool  string
	// OCIAuth selects how the provider authenticates: OCIAuthInstancePrincipal
	// for the instance's own certificate, or OCIAuthConfigFile (the default) for
	// an API key, which OCIConfigFile and OCIConfigProfile then locate.
	OCIAuth          string
	OCIConfigFile    string
	OCIConfigProfile string
	// Exoscale* configure a source="exoscale" pool: the zone the EIPs live in,
	// the instance they attach to, and how the exo CLI is invoked.
	ExoscaleZone       string
	ExoscaleInstanceID string
	ExoscaleConfigFile string
	ExoscaleAccount    string
	ExoscaleBinary     string
}

// IsDHCP reports whether the pool sources IPs from an upstream DHCP server.
func (p *ExternalPoolConfig) IsDHCP() bool {
	return p != nil && p.Source == SourceDHCP
}

// IsOCI reports whether the pool sources IPs from the Oracle Cloud API.
func (p *ExternalPoolConfig) IsOCI() bool {
	return p != nil && p.Source == SourceOCI
}

// UsesInstancePrincipal reports whether the OCI provider authenticates as the
// instance rather than from an API-key config file.
func (p *ExternalPoolConfig) UsesInstancePrincipal() bool {
	return p != nil && p.OCIAuth == OCIAuthInstancePrincipal
}

// IsExoscale reports whether the pool sources IPs from Exoscale Elastic IPs.
func (p *ExternalPoolConfig) IsExoscale() bool {
	return p != nil && p.Source == SourceExoscale
}

const (
	// SourceStatic is the default pool source (inline range math, KV-backed).
	SourceStatic = "static"
	// SourceDHCP delegates allocation to vpcd's DHCPManager.
	SourceDHCP = "dhcp"
	// SourceOCI delegates allocation to the Oracle Cloud API. An OCI VNIC
	// drops any source address that is not a registered private IP object on
	// it, so the addresses have to be created through the provider rather than
	// computed from a range.
	SourceOCI = "oci"
	// SourceExoscale delegates allocation to Exoscale Elastic IPs, created and
	// attached through the exo CLI. A manual EIP arrives addressed to itself,
	// so unlike OCI there is no private half to map.
	SourceExoscale = "exoscale"

	// OCIAuthConfigFile authenticates from an API-key config file. The default,
	// because it is what an operator already has from `oci setup config`.
	OCIAuthConfigFile = "config_file"
	// OCIAuthInstancePrincipal authenticates as the instance, from the
	// certificate its metadata service serves. No key material on any node and
	// nothing to rotate, at the cost of a dynamic group and an IAM policy.
	OCIAuthInstancePrincipal = "instance_principal"

	// DHCPMACDerived leases with deterministic per-client-id 02:xx MACs.
	DHCPMACDerived = "derived"
	// DHCPMACInterface leases with the bind interface's own MAC; the
	// upstream router keys on MAC, so client-id is the only discriminator.
	DHCPMACInterface = "interface"
)

// UsesIfaceMAC reports whether DHCP leases go out with the bind
// interface's own MAC instead of derived per-client-id MACs.
func (p *ExternalPoolConfig) UsesIfaceMAC() bool {
	return p != nil && p.DHCPMAC == DHCPMACInterface
}

// IGWSpec is the L5 input for IGWManager.AttachIGW / DetachIGW.
// InternetGatewayID is propagated into OVN external_ids for reconcile correlation.
type IGWSpec struct {
	VPCID             string
	InternetGatewayID string
	// RecordKey addresses the control-plane record this spec was loaded from,
	// so a successful attach can be reported back against it. Empty when the
	// spec did not come from the store.
	RecordKey string
	// AttachPending is set when the record still awaits confirmation, so a pass
	// reports one back only for an attachment that needs it.
	AttachPending bool
}
