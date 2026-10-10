package networkv1

const (
	// SubnetCreateSubject asks vpcd to ensure the subnet's logical switch
	// exists, pre-ensuring the VPC router first since vpc.create and
	// vpc.create-subnet carry no ordering guarantee. Fire-and-forget.
	SubnetCreateSubject = "vpc.create-subnet"

	// SubnetDeleteSubject asks vpcd to remove the subnet's logical switch.
	// Fire-and-forget.
	SubnetDeleteSubject = "vpc.delete-subnet"
)

// SubnetEvent is the payload on SubnetCreateSubject and SubnetDeleteSubject.
type SubnetEvent struct {
	SubnetId  string `json:"subnet_id"`
	VpcId     string `json:"vpc_id"`
	CidrBlock string `json:"cidr_block"`
}
