package subscribers

// NATS topic names for VPC lifecycle events published by daemon.
const (
	TopicVPCCreate        = "vpc.create"
	TopicVPCDelete        = "vpc.delete"
	TopicSubnetCreate     = "vpc.create-subnet"
	TopicSubnetDelete     = "vpc.delete-subnet"
	TopicCreatePort       = "vpc.create-port"
	TopicDeletePort       = "vpc.delete-port"
	TopicUpdatePortSGs    = "vpc.update-port-sgs"
	TopicIGWAttach        = "vpc.igw-attach"
	TopicIGWDetach        = "vpc.igw-detach"
	TopicAddNAT           = "vpc.add-nat"
	TopicDeleteNAT        = "vpc.delete-nat"
	TopicAddNATGateway    = "vpc.add-nat-gateway"
	TopicDeleteNATGateway = "vpc.delete-nat-gateway"
	TopicAddIGWRoute      = "vpc.add-igw-route"
	TopicDeleteIGWRoute   = "vpc.delete-igw-route"
	TopicCreateSG         = "vpc.create-sg"
	TopicDeleteSG         = "vpc.delete-sg"
	TopicUpdateSG         = "vpc.update-sg"
)

// VPCEvent is published on vpc.create / vpc.delete.
type VPCEvent struct {
	VpcId     string `json:"vpc_id"`
	CidrBlock string `json:"cidr_block"`
	VNI       int64  `json:"vni"`
}

// SubnetEvent is published on vpc.create-subnet / vpc.delete-subnet.
type SubnetEvent struct {
	SubnetId  string `json:"subnet_id"`
	VpcId     string `json:"vpc_id"`
	CidrBlock string `json:"cidr_block"`
}

// PortEvent: vpc.create-port / vpc.delete-port. SecurityGroupIds set on
// create so OVN PG membership is wired atomically with the LSP; empty on
// delete (handleDeletePort reads current memberships from the cache).
type PortEvent struct {
	NetworkInterfaceId string   `json:"network_interface_id"`
	SubnetId           string   `json:"subnet_id"`
	VpcId              string   `json:"vpc_id"`
	PrivateIpAddress   string   `json:"private_ip_address"`
	MacAddress         string   `json:"mac_address"`
	SecurityGroupIds   []string `json:"security_group_ids,omitempty"`
}

// UpdatePortSGsEvent: vpc.update-port-sgs. Declarative — vpcd diffs
// SecurityGroupIds against the current cache memberships.
type UpdatePortSGsEvent struct {
	NetworkInterfaceId string   `json:"network_interface_id"`
	PrivateIpAddress   string   `json:"private_ip_address"`
	SecurityGroupIds   []string `json:"security_group_ids"`
}

// NATEvent is published on vpc.add-nat / vpc.delete-nat for 1:1 public IP NAT.
type NATEvent struct {
	VpcId      string `json:"vpc_id"`
	ExternalIP string `json:"external_ip"`
	LogicalIP  string `json:"logical_ip"`
	PortName   string `json:"port_name"` // logical port for distributed NAT
	MAC        string `json:"mac"`       // external MAC for distributed NAT
}

// NATGatewayEvent is published on vpc.add-nat-gateway / vpc.delete-nat-gateway.
// SubnetId + DestinationCidr identify the per-subnet egress policy installed
// alongside the SNAT rule so the LR has a default route for the private
// subnet (priority SubnetEgressPriorityNATGW). Without that policy, SNAT
// rewrites the source IP but the packet has no route to leave the LR.
type NATGatewayEvent struct {
	VpcId           string `json:"vpc_id"`
	NatGatewayId    string `json:"nat_gateway_id"`
	PublicIp        string `json:"public_ip"`
	SubnetCidr      string `json:"subnet_cidr"`      // private subnet CIDR for SNAT logical_ip
	SubnetId        string `json:"subnet_id"`        // private subnet ID for inport policy match
	DestinationCidr string `json:"destination_cidr"` // route destination, typically 0.0.0.0/0
}

// IGWRouteEvent is published on vpc.add-igw-route / vpc.delete-igw-route
// when a route table associated with SubnetId carries (or loses) a route
// to InternetGatewayId for DestinationCidr. The subscriber installs (or
// removes) an OVN Logical_Router_Policy scoped to the subnet's inport.
type IGWRouteEvent struct {
	VpcId             string `json:"vpc_id"`
	SubnetId          string `json:"subnet_id"`
	DestinationCidr   string `json:"destination_cidr"`
	InternetGatewayId string `json:"internet_gateway_id"`
}

// SGRule mirrors handlers/ec2/vpc.SGRule on the wire (kept local to avoid
// the handler import).
type SGRule struct {
	IpProtocol string `json:"ip_protocol"`
	FromPort   int64  `json:"from_port"`
	ToPort     int64  `json:"to_port"`
	CidrIp     string `json:"cidr_ip,omitempty"`
	SourceSG   string `json:"source_sg,omitempty"`
}

// SGEvent mirrors handlers/ec2/vpc.SGEvent.
type SGEvent struct {
	GroupId      string   `json:"group_id"`
	VpcId        string   `json:"vpc_id"`
	IngressRules []SGRule `json:"ingress_rules,omitempty"`
	EgressRules  []SGRule `json:"egress_rules,omitempty"`
}
