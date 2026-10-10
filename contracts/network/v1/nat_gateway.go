package networkv1

const (
	// NATGatewayAddSubject asks vpcd to install the NAT Gateway's SNAT
	// rule, and the per-subnet egress policy when SubnetId is set.
	// Fire-and-forget.
	NATGatewayAddSubject = "vpc.add-nat-gateway"

	// NATGatewayDeleteSubject asks vpcd to remove the NAT Gateway's SNAT
	// rule and per-subnet egress policy. Fire-and-forget.
	NATGatewayDeleteSubject = "vpc.delete-nat-gateway"
)

// NATGatewayEvent is the payload on NATGatewayAddSubject and
// NATGatewayDeleteSubject. SubnetId and DestinationCidr are omitempty
// because one of today's two publishers (domains/ec2/natgw) never sets
// them; omitting matches that publisher's wire bytes exactly. See README.
type NATGatewayEvent struct {
	VpcId           string `json:"vpc_id"`
	NatGatewayId    string `json:"nat_gateway_id"`
	PublicIp        string `json:"public_ip"`
	SubnetCidr      string `json:"subnet_cidr"`
	SubnetId        string `json:"subnet_id,omitempty"`
	DestinationCidr string `json:"destination_cidr,omitempty"`
}
