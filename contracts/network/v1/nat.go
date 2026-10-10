package networkv1

const (
	// NATAddSubject asks vpcd to commit the OVN dnat_and_snat rule.
	// Request/reply, 45s timeout for the OVN flows barrier; some callers
	// publish it fire-and-forget instead. See README for both paths.
	NATAddSubject = "vpc.add-nat"

	// NATDeleteSubject asks vpcd to delete the dnat_and_snat rule and
	// unbind host plumbing. Request/reply, 15s timeout, retried on
	// ErrNoResponders. See README for the retry budget.
	NATDeleteSubject = "vpc.delete-nat"
)

// NATEvent is the payload on NATAddSubject and NATDeleteSubject.
type NATEvent struct {
	VpcId      string `json:"vpc_id"`
	ExternalIP string `json:"external_ip"`
	LogicalIP  string `json:"logical_ip"`
	PortName   string `json:"port_name"`
	MAC        string `json:"mac"`
}
