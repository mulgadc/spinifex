package handlers_imds

// MetaDataServerIP is the standard EC2 link-local metadata address.
const MetaDataServerIP = "169.254.169.254"

// VPCDNSServerIP is the link-local VPC DNS address served by the per-tap shim,
// the reserved co-tenant on the IMDS endpoint (both addresses are captured by
// the same demux flows). Guests receive it as their DHCP nameserver.
const VPCDNSServerIP = "169.254.169.253"

// HostBindAddrs are the host-side addresses a per-tap responder binds. They
// are the guest-facing addresses everywhere the host has no use for them
// itself, which is every bare-metal deployment.
//
// A cloud host is the exception: 169.254.169.254 is its own metadata service
// and 169.254.169.253 its own resolver, and a /32 on an ime- endpoint shadows
// both — so the node loses DNS and its cloud API the moment a guest starts.
// Binding elsewhere and DNAT'ing the guest's packets keeps the guest's view
// at 169.254.169.254 while leaving the host's routes alone.
type HostBindAddrs struct {
	Meta string
	DNS  string
}

// NewHostBindAddrs takes the configured pair, falling back to the guest-facing
// addresses when either is unset — half a remap would bind one address the
// guest can reach and one the host has already taken.
func NewHostBindAddrs(meta, dns string) HostBindAddrs {
	if meta == "" || dns == "" {
		return HostBindAddrs{Meta: MetaDataServerIP, DNS: VPCDNSServerIP}
	}
	return HostBindAddrs{Meta: meta, DNS: dns}
}

// Remapped reports whether the host serves these on addresses other than the
// ones the guest addresses, and so needs the DNAT that restores the fiction.
func (a HostBindAddrs) Remapped() bool {
	return a.Meta != MetaDataServerIP || a.DNS != VPCDNSServerIP
}

// pinnedVersion is the dated IMDS API version advertised by GET /.
const pinnedVersion = "2021-07-15"

// supportedVersions is the GET / listing. It is advertised only; normalizeVersion
// additionally accepts any dated-version prefix, so we honour more versions than
// we list — harmless because every version maps to the same /latest tree.
var supportedVersions = []string{pinnedVersion, "latest"}
