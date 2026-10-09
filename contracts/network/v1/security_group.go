package networkv1

const (
	// SecurityGroupCreateSubject asks vpcd to ensure the SG's port group
	// then apply its ACL set. Request/reply, 5s timeout.
	SecurityGroupCreateSubject = "vpc.create-sg"

	// SecurityGroupDeleteSubject asks vpcd to remove the SG's port group
	// and ACLs. Request/reply, 5s timeout, idempotent.
	SecurityGroupDeleteSubject = "vpc.delete-sg"

	// SecurityGroupUpdateSubject asks vpcd to replace the SG's ACL set.
	// Request/reply, 5s timeout; the port group is unaffected.
	SecurityGroupUpdateSubject = "vpc.update-sg"
)

// SecurityGroupRule is the subset of a security-group rule network needs to
// build an OVN ACL. Publishers may send more fields (rule ID, IPv6 CIDR,
// description, tags); see README for that asymmetry.
type SecurityGroupRule struct {
	IpProtocol string `json:"ip_protocol"`
	FromPort   int64  `json:"from_port"`
	ToPort     int64  `json:"to_port"`
	CidrIp     string `json:"cidr_ip,omitempty"`
	SourceSG   string `json:"source_sg,omitempty"`
}

// SecurityGroupEvent is the payload on SecurityGroupCreateSubject,
// SecurityGroupDeleteSubject and SecurityGroupUpdateSubject.
type SecurityGroupEvent struct {
	GroupId      string              `json:"group_id"`
	VpcId        string              `json:"vpc_id"`
	IngressRules []SecurityGroupRule `json:"ingress_rules,omitempty"`
	EgressRules  []SecurityGroupRule `json:"egress_rules,omitempty"`
}
