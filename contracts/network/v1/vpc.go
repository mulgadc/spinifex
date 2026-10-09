package networkv1

const (
	// VPCCreateSubject asks vpcd to ensure the VPC's logical router exists.
	// Fire-and-forget: the publisher does not wait for the ack envelope.
	VPCCreateSubject = "vpc.create"

	// VPCDeleteSubject asks vpcd to remove the VPC's logical router.
	// Fire-and-forget.
	VPCDeleteSubject = "vpc.delete"
)

// VPCEvent is the payload on VPCCreateSubject and VPCDeleteSubject.
// CidrBlock and VNI are meaningful on create only; delete needs only VpcId.
type VPCEvent struct {
	VpcId     string `json:"vpc_id"`
	CidrBlock string `json:"cidr_block"`
	VNI       int64  `json:"vni"`
}
