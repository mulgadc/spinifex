package policy

import (
	"context"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
)

// SGSpec is the policy-layer view of a security group. The OVN port group
// named by topology.SecurityGroupPortGroup(GroupID) must already exist.
type SGSpec struct {
	GroupID      string
	VPCID        string
	IngressRules []Rule
	EgressRules  []Rule
}

// SecurityGroupManager attaches ACL policy to an SG's port group.
type SecurityGroupManager interface {
	// EnsureSG replaces the full ACL set (infra + tenant) in one OVSDB
	// transaction; idempotent, so it serves both create and update.
	EnsureSG(ctx context.Context, sg SGSpec) error
}

// EgressPolicy is the platform egress-abuse policy applied to every guest port
// group regardless of the tenant's security groups. The zero value blocks
// nothing.
type EgressPolicy struct {
	// BlockedWANPorts are TCP destination ports dropped to public destinations
	// (AWS-parity outbound SMTP block). Empty disables the block.
	BlockedWANPorts []int
	// ExemptVPCs are VPC IDs the block is not applied to — the operator
	// workaround for a tenant with a legitimate need until per-account
	// exceptions exist.
	ExemptVPCs map[string]bool
}

type sgManager struct {
	ovn    ovn.Client
	egress EgressPolicy
}

var _ SecurityGroupManager = (*sgManager)(nil)

// NewSecurityGroupManager constructs a SecurityGroupManager backed by client.
// egress carries the platform WAN egress block applied to every guest.
func NewSecurityGroupManager(client ovn.Client, egress EgressPolicy) SecurityGroupManager {
	return &sgManager{ovn: client, egress: egress}
}

func (m *sgManager) EnsureSG(ctx context.Context, sg SGSpec) error {
	return m.applyACLs(ctx, sg)
}

// applyACLs atomically replaces infra + tenant ACLs in one OVSDB transaction.
// ClearACLs+AddACLs split left zero-ACL gaps that defaulted traffic to drop.
func (m *sgManager) applyACLs(ctx context.Context, sg SGSpec) error {
	pg := topology.SecurityGroupPortGroup(sg.GroupID)
	ruleSpecs, err := RuleACLSpecs(pg, sg.IngressRules, sg.EgressRules)
	if err != nil {
		return fmt.Errorf("build ACLs for %s: %w", pg, err)
	}
	specs := append(InfrastructureACLs(pg), ruleSpecs...)
	// Platform WAN egress block: applied to every guest unless its VPC is
	// exempted, at a priority a tenant SG allow cannot override.
	if !m.egress.ExemptVPCs[sg.VPCID] {
		if block, ok := BlockedWANEgressACL(pg, m.egress.BlockedWANPorts); ok {
			specs = append(specs, block)
		}
	}
	if err := m.ovn.ReplaceACLs(ctx, pg, specs); err != nil {
		return fmt.Errorf("replace ACLs on %s: %w", pg, err)
	}
	return nil
}
