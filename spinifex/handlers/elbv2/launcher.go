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
type SystemInstanceInput struct {
	InstanceType string // e.g. "sys.micro"
	SubnetID     string // VPC subnet for networking

	// ENI fields — the VM always uses a pre-created ENI (the ALB's primary ENI).
	ENIID  string
	ENIMac string
	ENIIP  string

	// ExtraENIs lists additional pre-created ENIs that should be attached to
	// the VM alongside the primary ENI. Used for multi-subnet ALBs so the VM
	// has a NIC (and tap on br-int) in every subnet the LB spans.
	ExtraENIs []ExtraENIInput

	// Scheme is the ALB scheme ("internet-facing" or "internal").
	// Internet-facing ALBs get a public IP and NAT rules; internal ALBs do not.
	Scheme string

	// AccountID is the owner account of the ALB's ENI. Required so the daemon
	// can look up and update the ENI record (which is keyed by account).
	AccountID string

	// HostfwdPorts specifies additional guest ports to forward from the host
	// via the QEMU user-mode dev NIC (dev_networking mode only).
	// Each entry is a guest port (e.g. 80, 443). The host port is auto-assigned.
	HostfwdPorts []int

	// NICs defines the network interfaces for the microVM.
	// Index 0 is the primary VPC ENI, index 1 is the management NIC, index 2+ are extra ENIs.
	NICs []NICConfig

	// LBAgentEnv is a KEY=value blob written to /etc/conf.d/lb-agent inside
	// the guest via fw_cfg.
	LBAgentEnv string

	// CACert holds PEM-encoded CA certificate bytes delivered to the guest via fw_cfg.
	CACert string
}

// ExtraENIInput describes an additional pre-created ENI to attach to a
// system instance. The primary ENI is still passed via ENIID/ENIMac/ENIIP.
type ExtraENIInput struct {
	ENIID    string
	ENIMac   string
	ENIIP    string
	SubnetID string
}

// NICConfig describes a single network interface for a microVM direct-boot launch.
// Index 0 is the primary VPC ENI, index 1 is the management NIC, index 2+ are extra ENIs.
type NICConfig struct {
	MAC       string // e.g. "02:0a:01:23:45:67"
	CIDR      string // e.g. "10.0.1.5/24"
	Gateway   string // e.g. "10.0.1.1"; empty for mgmt NIC
	IsDefault bool   // true for exactly one NIC (primary VPC ENI)
	RouteDst  string // specific host route dst, e.g. "10.20.0.5/32"
	RouteVia  string // next-hop for RouteDst
}

// SystemInstanceOutput contains the result of a successful launch.
type SystemInstanceOutput struct {
	InstanceID string // e.g. "i-xxxxx"
	PrivateIP  string // VPC private IP
	PublicIP   string // Public IP (only for internet-facing scheme)

	// HostfwdMap maps guest port → host port for any forwarded ports.
	// Only populated when dev_networking is enabled and HostfwdPorts were requested.
	HostfwdMap map[int]int
}
