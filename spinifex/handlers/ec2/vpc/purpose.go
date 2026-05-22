package handlers_ec2_vpc

// Purpose tags every IP allocation with its semantic role so multi-VPC
// clusters can reclaim / audit by (owner, purpose). The constants match the
// JSON values written to KV records (mulga-siv-125.3.3 Slice B).
const (
	PurposeENIPrimary    = "eni-primary"
	PurposeENISecondary  = "eni-secondary"
	PurposeENIPublic     = "eni-public" // auto-assigned public IP
	PurposeEIP           = "eip"
	PurposeNATGWExternal = "natgw-external"
	PurposeIGWLRP        = "igw-lrp"
	// Future-reserved values; defined now so call sites and migrations can
	// reference them without a follow-up rename.
	PurposeEdgeLRExternal = "edge-lr-external" // 125.3.6
	PurposeNodeMgmt       = "node-mgmt"        // currently .env
	PurposeSubnetRouter   = "subnet-router"    // .1
	PurposeSubnetDNS      = "subnet-dns"       // .2
	PurposeSubnetReserved = "subnet-reserved"  // .0, .3, .last
	// PurposeUnknown tags records the migration could not classify (e.g.
	// orphan IPs with no owning ENIRecord). Surfaces in logs for triage.
	PurposeUnknown = "unknown"
)

// LegacyExternalTypeToPurpose maps the old ExternalIPAllocation.Type values
// onto the new Purpose enum. Used by the v1→v2 external-IPAM migration.
var LegacyExternalTypeToPurpose = map[string]string{
	"gateway":     PurposeIGWLRP,
	"auto_assign": PurposeENIPublic,
	"elastic_ip":  PurposeEIP,
}
