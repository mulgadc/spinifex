// Package sysinstance holds the boot-agnostic contract for system-managed VMs.
// Hidden from DescribeInstances; supports two boot styles: BootDirect (fw_cfg microVM)
// and BootAMI (AMI + cloud-init). Types live here to avoid an eks→elbv2 import cycle.
package sysinstance

import "errors"

// ErrSystemInstanceNotFound is returned by TerminateSystemInstance when the instance is unknown.
// Callers treat it as idempotent success so a retried delete does not block on a gone VM.
var ErrSystemInstanceNotFound = errors.New("sysinstance: instance not found")

// BootMode selects how a system instance boots.
type BootMode string

const (
	// BootDirect is the direct-boot microVM path (fw_cfg-delivered config).
	BootDirect BootMode = "direct"
	// BootAMI is the AMI + cloud-init path (root volume + seed ISO).
	BootAMI BootMode = "ami"
)

// SystemInstanceLauncher launches and terminates system-managed VMs.
// Defined here to avoid a circular import between elbv2/eks and daemon.
type SystemInstanceLauncher interface {
	// LaunchSystemInstance creates and starts a system-managed VM, returning its ID and private IP.
	LaunchSystemInstance(input *SystemInstanceInput) (*SystemInstanceOutput, error)

	// TerminateSystemInstance stops and cleans up a system-managed VM.
	TerminateSystemInstance(instanceID string) error
}

// SystemInstanceInput describes a system VM to launch.
// BootDirect uses NICs/LBAgentEnv/CACert; BootAMI uses ImageID/UserData.
// JSON tags are explicit as this struct crosses NATS subjects.
type SystemInstanceInput struct {
	// BootMode selects the boot style; empty defaults to BootDirect.
	BootMode BootMode `json:"boot_mode,omitempty"`

	// ManagedBy is the spinifex:managed-by tag value (e.g. "elbv2", "eks").
	ManagedBy string `json:"managed_by,omitempty"`

	InstanceType string `json:"instance_type"` // e.g. "sys.micro"
	SubnetID     string `json:"subnet_id"`     // VPC subnet for networking

	ENIID  string `json:"eni_id"`
	ENIMac string `json:"eni_mac"`
	ENIIP  string `json:"eni_ip"`

	// ExtraENIs lists additional pre-created ENIs to attach alongside the
	// primary ENI (e.g. multi-subnet ALBs).
	ExtraENIs []ExtraENIInput `json:"extra_enis,omitempty"`

	// Scheme is the ALB scheme ("internet-facing" or "internal"). BootDirect
	// only; ignored for BootAMI.
	Scheme string `json:"scheme,omitempty"`

	// AccountID is the owner account of the primary ENI. Required so the daemon
	// can look up and update the ENI record (which is keyed by account).
	AccountID string `json:"account_id"`

	// HostfwdPorts forwards additional guest ports from the host via the QEMU
	// user-mode dev NIC (dev_networking mode only).
	HostfwdPorts []int `json:"hostfwd_ports,omitempty"`

	// NICs defines the network interfaces for a BootDirect microVM. Index 0 is
	// the primary VPC ENI, index 1 the mgmt NIC, index 2+ extra ENIs.
	NICs []NICConfig `json:"nics,omitempty"`

	// LBAgentEnv is a KEY=value blob written to /etc/conf.d/lb-agent inside a
	// BootDirect guest via fw_cfg.
	LBAgentEnv string `json:"lb_agent_env,omitempty"`

	// CACert holds PEM CA bytes delivered to a BootDirect guest via fw_cfg.
	CACert string `json:"ca_cert,omitempty"`

	// ImageID is the AMI to clone the root volume from (BootAMI only).
	ImageID string `json:"image_id,omitempty"`

	// UserData is the raw (un-encoded) cloud-init user-data for a BootAMI guest.
	// The launcher base64-encodes it for the RunInstances path.
	UserData string `json:"user_data,omitempty"`
}

// ExtraENIInput describes an additional pre-created ENI to attach to a system
// instance. The primary ENI is still passed via ENIID/ENIMac/ENIIP.
type ExtraENIInput struct {
	ENIID    string `json:"eni_id"`
	ENIMac   string `json:"eni_mac"`
	ENIIP    string `json:"eni_ip"`
	SubnetID string `json:"subnet_id"`
}

// NICConfig describes a single network interface for a BootDirect microVM.
// Index 0 is the primary VPC ENI, index 1 the management NIC, index 2+ extra.
type NICConfig struct {
	MAC       string `json:"mac"`                 // e.g. "02:0a:01:23:45:67"
	CIDR      string `json:"cidr"`                // e.g. "10.0.1.5/24"
	Gateway   string `json:"gateway,omitempty"`   // e.g. "10.0.1.1"; empty for mgmt NIC
	IsDefault bool   `json:"is_default"`          // true for exactly one NIC (primary VPC ENI)
	RouteDst  string `json:"route_dst,omitempty"` // specific host route dst, e.g. "10.20.0.5/32"
	RouteVia  string `json:"route_via,omitempty"` // next-hop for RouteDst
}

// RecoveryContext carries the per-VM fields needed to reconstruct a
// SystemInstanceInput during host-reboot recovery without importing the vm
// package. MgmtMAC / MgmtIP are the values the daemon already allocated on a
// prior launch; the service injects them back into NIC[1] rather than
// re-allocating from the mgmt IPAM.
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
	PublicIP   string `json:"public_ip,omitempty"` // only for internet-facing scheme
	MgmtIP     string `json:"mgmt_ip,omitempty"`   // br-mgmt NIC IP, reachable from the host

	// HostfwdMap maps guest port → host port for any forwarded ports.
	HostfwdMap map[int]int `json:"hostfwd_map,omitempty"`
}
