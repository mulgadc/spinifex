package networkv1

const (
	// InternetGatewayAttachSubject asks vpcd to build the OVN external
	// switch, gateway and SNAT. Fire-and-forget, non-fatal on failure.
	InternetGatewayAttachSubject = "vpc.igw-attach"

	// InternetGatewayDetachSubject asks vpcd to tear down the OVN external
	// switch, gateway and NAT. Fire-and-forget, non-fatal on failure.
	InternetGatewayDetachSubject = "vpc.igw-detach"
)

// InternetGatewayEvent is the payload on InternetGatewayAttachSubject and
// InternetGatewayDetachSubject.
type InternetGatewayEvent struct {
	InternetGatewayId string `json:"internet_gateway_id"`
	VpcId             string `json:"vpc_id"`
}
