package handlers_imds

// MetaDataServerIP is the standard EC2 link-local metadata address.
const MetaDataServerIP = "169.254.169.254"

// pinnedVersion is the dated IMDS API version advertised by GET /.
const pinnedVersion = "2021-07-15"

// supportedVersions is the GET / listing. It is advertised only; normalizeVersion
// additionally accepts any dated-version prefix, so we honour more versions than
// we list — harmless because every version maps to the same /latest tree.
var supportedVersions = []string{pinnedVersion, "latest"}
