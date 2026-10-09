package networkv1

const (
	// IGWRouteAddSubject asks vpcd to install the per-subnet egress policy
	// for a route-table route to an internet gateway. Fire-and-forget.
	IGWRouteAddSubject = "vpc.add-igw-route"

	// IGWRouteDeleteSubject asks vpcd to remove that egress policy.
	// Fire-and-forget.
	IGWRouteDeleteSubject = "vpc.delete-igw-route"
)

// IGWRouteEvent is the payload on IGWRouteAddSubject and
// IGWRouteDeleteSubject.
type IGWRouteEvent struct {
	VpcId             string `json:"vpc_id"`
	SubnetId          string `json:"subnet_id"`
	DestinationCidr   string `json:"destination_cidr"`
	InternetGatewayId string `json:"internet_gateway_id"`
}
