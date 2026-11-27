package vpc

// Purpose tags every IP allocation with its semantic role so multi-VPC
// clusters can reclaim / audit by (owner, purpose). The constants match the
// JSON values written to KV records.
const (
	PurposeENIPrimary = "eni-primary"
	PurposeENIPublic  = "eni-public" // auto-assigned public IP
	PurposeEIP        = "eip"
	PurposeIGWLRP     = "igw-lrp"
	// PurposeLBVIP is a private load balancer address. Only its owner's exact
	// mapping token may release it, so the legacy operations refuse it.
	PurposeLBVIP = "lb-vip"
)
