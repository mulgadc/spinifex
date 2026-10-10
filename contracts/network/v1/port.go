package networkv1

const (
	// PortCreateSubject asks vpcd to create the port's OVN logical switch
	// port. Request/reply so an OVSDB failure surfaces to the caller rather
	// than being swallowed; the deployed timeout is 5s.
	PortCreateSubject = "vpc.create-port"

	// PortDeleteSubject asks vpcd to remove the port's OVN logical switch
	// port. Request/reply, 5s timeout, but the deployed caller treats
	// failure as non-fatal and relies on the reconciler's orphan prune.
	PortDeleteSubject = "vpc.delete-port"
)

// PortEvent is the payload on PortCreateSubject and PortDeleteSubject.
// SecurityGroupIds and SuppressDHCP are meaningful on create only; delete
// carries them zero-valued.
type PortEvent struct {
	NetworkInterfaceId string   `json:"network_interface_id"`
	SubnetId           string   `json:"subnet_id"`
	VpcId              string   `json:"vpc_id"`
	PrivateIpAddress   string   `json:"private_ip_address"`
	MacAddress         string   `json:"mac_address"`
	SecurityGroupIds   []string `json:"security_group_ids,omitempty"`
	SuppressDHCP       bool     `json:"suppress_dhcp,omitempty"`
}
