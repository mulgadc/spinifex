package handlers_elbv2

// SystemInstanceLauncher is the subset of daemon functionality needed by
// ELBv2 to launch and terminate system-managed VMs (e.g. ALB VMs).
// Defined here to avoid a circular import between handlers/elbv2 and daemon.
//
// System instances are owned by the system account and hidden from
// customer DescribeInstances calls.
type SystemInstanceLauncher interface {
	// LaunchSystemInstance creates and starts a system-managed VM.
	// The VM is assigned to the system account and not visible to customers.
	// The caller provides the instance type, AMI, subnet, user data, and
	// optionally a pre-created ENI to attach.
	//
	// Returns the instance ID and private IP once the VM is running.
	LaunchSystemInstance(input *SystemInstanceInput) (*SystemInstanceOutput, error)

	// TerminateSystemInstance stops and cleans up a system-managed VM.
	TerminateSystemInstance(instanceID string) error
}

// SystemInstanceInput describes the direct-boot microVM to launch for an
// ELBv2 load balancer. There is no AMI or cloud-init path — kernel and
// initrd come from the bundled spinifex package and per-VM config is
// delivered via QEMU fw_cfg blobs.
//
// JSON tags are explicit because this struct crosses the
// system.LaunchInstance.* NATS subject root when ELBv2 hands a launch off
// to the cluster — see daemon/daemon_system_dispatch.go.
type SystemInstanceInput struct {
	InstanceType string `json:"instance_type"` // e.g. "sys.micro"
	SubnetID     string `json:"subnet_id"`     // VPC subnet for networking

	// ENI fields — the VM always uses a pre-created ENI (the ALB's primary ENI).
	ENIID  string `json:"eni_id"`
	ENIMac string `json:"eni_mac"`
	ENIIP  string `json:"eni_ip"`

	// ExtraENIs lists additional pre-created ENIs that should be attached to
	// the VM alongside the primary ENI. Used for multi-subnet ALBs so the VM
	// has a NIC (and tap on br-int) in every subnet the LB spans.
	ExtraENIs []ExtraENIInput `json:"extra_enis,omitempty"`

	// Scheme is the ALB scheme ("internet-facing" or "internal").
	// Internet-facing ALBs get a public IP and NAT rules; internal ALBs do not.
	Scheme string `json:"scheme"`

	// AccountID is the owner account of the ALB's ENI. Required so the daemon
	// can look up and update the ENI record (which is keyed by account).
	AccountID string `json:"account_id"`

	// HostfwdPorts specifies additional guest ports to forward from the host
	// via the QEMU user-mode dev NIC (dev_networking mode only).
	// Each entry is a guest port (e.g. 80, 443). The host port is auto-assigned.
	HostfwdPorts []int `json:"hostfwd_ports,omitempty"`

	// NICs defines the network interfaces for the microVM.
	// Index 0 is the primary VPC ENI, index 1 is the management NIC, index 2+ are extra ENIs.
	NICs []NICConfig `json:"nics,omitempty"`

	// LBAgentEnv is a KEY=value blob written to /etc/conf.d/lb-agent inside
	// the guest via fw_cfg.
	LBAgentEnv string `json:"lb_agent_env"`

	// CACert holds PEM-encoded CA certificate bytes delivered to the guest via fw_cfg.
	CACert string `json:"ca_cert"`
}

// ExtraENIInput describes an additional pre-created ENI to attach to a
// system instance. The primary ENI is still passed via ENIID/ENIMac/ENIIP.
type ExtraENIInput struct {
	ENIID    string `json:"eni_id"`
	ENIMac   string `json:"eni_mac"`
	ENIIP    string `json:"eni_ip"`
	SubnetID string `json:"subnet_id"`
}

// NICConfig describes a single network interface for a microVM direct-boot launch.
// Index 0 is the primary VPC ENI, index 1 is the management NIC, index 2+ are extra ENIs.
type NICConfig struct {
	MAC       string `json:"mac"`                 // e.g. "02:0a:01:23:45:67"
	CIDR      string `json:"cidr"`                // e.g. "10.0.1.5/24"
	Gateway   string `json:"gateway,omitempty"`   // e.g. "10.0.1.1"; empty for mgmt NIC
	IsDefault bool   `json:"is_default"`          // true for exactly one NIC (primary VPC ENI)
	RouteDst  string `json:"route_dst,omitempty"` // specific host route dst, e.g. "10.20.0.5/32"
	RouteVia  string `json:"route_via,omitempty"` // next-hop for RouteDst
}

// RecoveryContext carries the per-VM fields the ELBv2 service needs to
// reconstruct a SystemInstanceInput during host-reboot recovery, without
// importing the vm package (which would cycle back through daemon).
// MgmtMAC / MgmtIP are the values the daemon already allocated for this
// instance on a prior launch; the service injects them back into NIC[1]
// rather than re-allocating from the mgmt IPAM.
type RecoveryContext struct {
	InstanceID   string
	InstanceType string
	ENIMac       string
	MgmtMAC      string
	MgmtIP       string
}

// SystemInstanceOutput contains the result of a successful launch.
type SystemInstanceOutput struct {
	InstanceID string `json:"instance_id"`         // e.g. "i-xxxxx"
	PrivateIP  string `json:"private_ip"`          // VPC private IP
	PublicIP   string `json:"public_ip,omitempty"` // Public IP (only for internet-facing scheme)

	// HostfwdMap maps guest port → host port for any forwarded ports.
	// Only populated when dev_networking is enabled and HostfwdPorts were requested.
	HostfwdMap map[int]int `json:"hostfwd_map,omitempty"`
}
