package handlers_imds

// MetaDataServerIP is the standard EC2 link-local metadata address.
const MetaDataServerIP = "169.254.169.254"

// VPCDNSServerIP is the link-local VPC DNS address served by the per-tap shim,
// the reserved co-tenant on the IMDS endpoint (both addresses are captured by
// the same demux flows). Guests receive it as their DHCP nameserver.
const VPCDNSServerIP = "169.254.169.253"

// pinnedVersion is the dated IMDS API version advertised by GET /.
const pinnedVersion = "2021-07-15"

// supportedVersions is the GET / listing. It is advertised only; normalizeVersion
// additionally accepts any dated-version prefix, so we honour more versions than
// we list — harmless because every version maps to the same /latest tree.
var supportedVersions = []string{pinnedVersion, "latest"}
