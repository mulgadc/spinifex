package handlers_ec2_vpc

// Purpose tags every IP allocation with its semantic role so multi-VPC
// clusters can reclaim / audit by (owner, purpose). The constants match the
// JSON values written to KV records.
const (
	PurposeENIPrimary    = "eni-primary"
	PurposeENISecondary  = "eni-secondary"
	PurposeENIPublic     = "eni-public" // auto-assigned public IP
	PurposeEIP           = "eip"
	PurposeNATGWExternal = "natgw-external"
	PurposeIGWLRP        = "igw-lrp"
)

// LegacyExternalTypeToPurpose maps the old ExternalIPAllocation.Type values
// onto the new Purpose enum. Used by the v1→v2 external-IPAM migration.
var LegacyExternalTypeToPurpose = map[string]string{
	"gateway":     PurposeIGWLRP,
	"auto_assign": PurposeENIPublic,
	"elastic_ip":  PurposeEIP,
}
