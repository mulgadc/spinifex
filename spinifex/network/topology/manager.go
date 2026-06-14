// Package topology is L2 of the spinifex network stack. It translates
// VPC/subnet/ENI/SG objects into OVN logical network objects via L1.
// Higher layers (policy, external, federation) build on this.
package topology

import (
	"context"
	"net"
	"net/netip"
)

// Manager owns the lifecycle of OVN logical objects for AWS-style API
// resources. Implementations must be idempotent.
type Manager interface {
	EnsureVPC(ctx context.Context, vpc VPCSpec) error
	DeleteVPC(ctx context.Context, vpcID string) error

	EnsureSubnet(ctx context.Context, subnet SubnetSpec) error
	DeleteSubnet(ctx context.Context, subnet SubnetSpec) error

	EnsurePort(ctx context.Context, port PortSpec) error
	DeletePort(ctx context.Context, port PortSpec) error

	// SetPortSecurityGroups applies the declarative set of port-group
	// memberships, computing the add/remove diff against current OVN state.
	SetPortSecurityGroups(ctx context.Context, portID string, sgIDs []string) error

	// SG port-group lifecycle. ACL programming lives in network/policy.
	EnsureSGPortGroup(ctx context.Context, groupID string) error
	DeleteSGPortGroup(ctx context.Context, groupID string) error

	// DeleteSGPortGroupByName tears down a port group by raw OVN name (e.g.
	// "sg_abc"). Used by the reconciler's orphan-removal path.
	DeleteSGPortGroupByName(ctx context.Context, pgName string) error
}

// VPCSpec describes a VPC at L2.
type VPCSpec struct {
	VPCID string
	CIDR  netip.Prefix
	VNI   int64
}

// SubnetSpec describes a subnet at L2. CIDR must be contained in the parent
// VPC's CIDR (API-layer validation).
type SubnetSpec struct {
	SubnetID string
	VPCID    string
	CIDR     netip.Prefix
}

// PortSpec describes an ENI port at L2. PrivateIP+MAC bind to LSP Addresses/
// PortSecurity at create; SG membership changes go through SetPortSecurityGroups.
type PortSpec struct {
	PortID    string
	SubnetID  string
	VPCID     string
	PrivateIP netip.Addr
	MAC       net.HardwareAddr
	SGIDs     []string
	// PublicIP is the ENI's auto-assigned or ELB public address, when present.
	// It does not affect the L2 LSP; the reconciler reads it to exempt the
	// instance from its subnet egress drop gate (the dnat_and_snat datapath).
	PublicIP netip.Addr
}
