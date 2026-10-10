package networkv1

// PortSecurityGroupsUpdateSubject asks vpcd to reconcile a port's OVN
// port-group memberships against a declarative list; vpcd computes the
// diff. Request/reply, 5s timeout, errors propagate to the caller.
const PortSecurityGroupsUpdateSubject = "vpc.update-port-sgs"

// PortSecurityGroupsUpdateEvent is the payload on
// PortSecurityGroupsUpdateSubject.
type PortSecurityGroupsUpdateEvent struct {
	NetworkInterfaceId string   `json:"network_interface_id"`
	PrivateIpAddress   string   `json:"private_ip_address"`
	SecurityGroupIds   []string `json:"security_group_ids"`
}
