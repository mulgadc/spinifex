package ec2v1

const (
	// InternetGatewayAttachSubject projects an EC2 internet-gateway attachment
	// into the network data plane.
	InternetGatewayAttachSubject = "vpc.igw-attach"

	// InternetGatewayDetachSubject projects an EC2 internet-gateway detachment
	// into the network data plane.
	InternetGatewayDetachSubject = "vpc.igw-detach"
)

// InternetGatewayEvent is the EC2-to-network projection carried on
// InternetGatewayAttachSubject and InternetGatewayDetachSubject. Its JSON
// fields are a cross-process compatibility boundary.
type InternetGatewayEvent struct {
	InternetGatewayId string `json:"internet_gateway_id"`
	VpcId             string `json:"vpc_id"`
}
