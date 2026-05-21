// Package topology is L2 of the spinifex network stack: the logical network
// model. It translates VPC/subnet/ENI/SG API objects into OVN logical network
// objects via L1 (network/ovn). Higher layers (policy, external, federation)
// build on this; lower layers must not call here.
//
// See docs/development/feature/spinifex-network-redesign.md §7 and
// docs/development/proposals/az-local-architecture/0006-spinifex-network-layer-contract.md
// for the full contract.
package topology

import (
	"context"
	"net"
	"net/netip"
)

// Manager owns the lifecycle of OVN logical objects for AWS-style API
// resources. Implementations must be idempotent: a second EnsureX call with
// the same spec is a no-op.
type Manager interface {
	// VPC lifecycle
	EnsureVPC(ctx context.Context, vpc VPCSpec) error
	DeleteVPC(ctx context.Context, vpcID string) error

	// Subnet lifecycle
	EnsureSubnet(ctx context.Context, subnet SubnetSpec) error
	DeleteSubnet(ctx context.Context, subnet SubnetSpec) error

	// Port lifecycle (ENI)
	EnsurePort(ctx context.Context, port PortSpec) error
	DeletePort(ctx context.Context, port PortSpec) error

	// SetPortSecurityGroups applies the declarative set of port-group
	// memberships for a port. Manager computes the add/remove diff against
	// current OVN state.
	SetPortSecurityGroups(ctx context.Context, portID string, sgIDs []string) error

	// SG port-group lifecycle. ACL programming on a port group lives in
	// network/policy.SecurityGroupManager — topology only owns the empty
	// OVN port-group row keyed by SecurityGroupPortGroup(groupID).
	EnsureSGPortGroup(ctx context.Context, groupID string) error
	DeleteSGPortGroup(ctx context.Context, groupID string) error
}

// VPCSpec describes a VPC at L2. AZSlice is reserved for Phase 3 per-AZ CIDR
// slicing; Phase 1–2 leave it zero and treat CIDR as the full VPC range.
type VPCSpec struct {
	VPCID string
	CIDR  netip.Prefix
	VNI   int64
}

// SubnetSpec describes a subnet at L2. CIDR must be contained in the parent
// VPC's CIDR (or AZSlice in Phase 3); API-layer validation enforces that.
type SubnetSpec struct {
	SubnetID string
	VPCID    string
	CIDR     netip.Prefix
}

// PortSpec describes an ENI / VM port at L2. PrivateIP and MAC are bound to
// the LSP's Addresses + PortSecurity columns at create time. SGIDs is the
// initial port-group membership; subsequent changes go through
// SetPortSecurityGroups.
type PortSpec struct {
	PortID    string
	SubnetID  string
	VPCID     string
	PrivateIP netip.Addr
	MAC       net.HardwareAddr
	SGIDs     []string
}
