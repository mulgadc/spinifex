package networkv1

const (
	// SubnetEgressGateSubject asks vpcd to install a DROP policy for a
	// subnet that lost its default egress target. Fire-and-forget.
	SubnetEgressGateSubject = "vpc.gate-subnet-egress"

	// SubnetEgressUngateSubject asks vpcd to remove that DROP policy once
	// the subnet regains an egress target. Fire-and-forget.
	SubnetEgressUngateSubject = "vpc.ungate-subnet-egress"
)

// SubnetEgressGateEvent is the payload on SubnetEgressGateSubject.
type SubnetEgressGateEvent struct {
	VpcId           string `json:"vpc_id"`
	SubnetId        string `json:"subnet_id"`
	DestinationCidr string `json:"destination_cidr"`
}

// SubnetEgressUngateEvent is the payload on SubnetEgressUngateSubject.
// Identical shape to SubnetEgressGateEvent, kept distinct because the two
// routes are opposite decisions, not variants of one event.
type SubnetEgressUngateEvent struct {
	VpcId           string `json:"vpc_id"`
	SubnetId        string `json:"subnet_id"`
	DestinationCidr string `json:"destination_cidr"`
}
