package policy

import (
	"context"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

// SGSpec is the policy-layer view of a security group. The OVN port group
// named by topology.SecurityGroupPortGroup(GroupID) must already exist.
type SGSpec struct {
	GroupID      string
	VPCID        string
	IngressRules []Rule
	EgressRules  []Rule
}

// SecurityGroupManager attaches ACL policy to an SG's port group. EnsureSG
// and UpdateSG share replace-all semantics: each call clears then re-adds
// the full ACL set (infra + tenant) atomically per AddACLs transaction.
type SecurityGroupManager interface {
	// EnsureSG sets the full ACL set; idempotent.
	EnsureSG(ctx context.Context, sg SGSpec) error

	// UpdateSG replaces the SG's ACL set. Identical to EnsureSG; kept
	// distinct to mirror §8.1 of the parent plan.
	UpdateSG(ctx context.Context, sg SGSpec) error

	// DeleteSG clears every ACL on the SG's port group; the PG itself
	// stays (topology.Manager owns the lifecycle).
	DeleteSG(ctx context.Context, groupID string) error
}

type sgManager struct {
	ovn ovn.Client
}

var _ SecurityGroupManager = (*sgManager)(nil)

// NewSecurityGroupManager constructs a SecurityGroupManager backed by client.
func NewSecurityGroupManager(client ovn.Client) SecurityGroupManager {
	return &sgManager{ovn: client}
}

func (m *sgManager) EnsureSG(ctx context.Context, sg SGSpec) error {
	return m.applyACLs(ctx, sg)
}

func (m *sgManager) UpdateSG(ctx context.Context, sg SGSpec) error {
	return m.applyACLs(ctx, sg)
}

func (m *sgManager) DeleteSG(ctx context.Context, groupID string) error {
	pg := topology.SecurityGroupPortGroup(groupID)
	if err := m.ovn.ClearACLs(ctx, pg); err != nil {
		return fmt.Errorf("clear ACLs on %s: %w", pg, err)
	}
	return nil
}

// applyACLs clears the PG and re-adds infra + tenant ACLs. ClearACLs +
// AddACLs is not one OVSDB transaction; the observable gap is bounded by
// southbound flow-install latency.
func (m *sgManager) applyACLs(ctx context.Context, sg SGSpec) error {
	pg := topology.SecurityGroupPortGroup(sg.GroupID)
	if err := m.ovn.ClearACLs(ctx, pg); err != nil {
		return fmt.Errorf("clear ACLs on %s: %w", pg, err)
	}
	specs := append(InfrastructureACLs(pg), RuleACLSpecs(pg, sg.IngressRules, sg.EgressRules)...)
	if err := m.ovn.AddACLs(ctx, pg, specs); err != nil {
		return fmt.Errorf("add ACLs on %s: %w", pg, err)
	}
	return nil
}
