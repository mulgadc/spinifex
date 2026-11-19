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

// SecurityGroupRule is a security-group rule as EC2 stores it. Network's ACL
// builder only reads IpProtocol, the ports, CidrIp, SourceSG and PrefixListId;
// the rest exist so the wire shape matches the publisher's stored record.
type SecurityGroupRule struct {
	RuleId     string `json:"rule_id"`
	IpProtocol string `json:"ip_protocol"`
	FromPort   int64  `json:"from_port"`
	ToPort     int64  `json:"to_port"`
	CidrIp     string `json:"cidr_ip,omitempty"`
	CidrIpv6   string `json:"cidr_ipv6,omitempty"`
	SourceSG   string `json:"source_sg,omitempty"`
	// PrefixListId is omitted when empty, so a consumer that predates it reads
	// a rule naming one as having no source and drops it.
	PrefixListId string            `json:"prefix_list_id,omitempty"`
	Description  string            `json:"description,omitempty"`
	Tags         map[string]string `json:"tags,omitempty"`
}

// SecurityGroupEvent is the payload on SecurityGroupCreateSubject,
// SecurityGroupDeleteSubject and SecurityGroupUpdateSubject.
type SecurityGroupEvent struct {
	GroupId      string              `json:"group_id"`
	VpcId        string              `json:"vpc_id"`
	IngressRules []SecurityGroupRule `json:"ingress_rules,omitempty"`
	EgressRules  []SecurityGroupRule `json:"egress_rules,omitempty"`
}
