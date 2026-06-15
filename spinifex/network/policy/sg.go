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
// and UpdateSG share replace-all semantics: each call replaces the full ACL
// set (infra + tenant) in one OVSDB transaction.
type SecurityGroupManager interface {
	// EnsureSG sets the full ACL set; idempotent.
	EnsureSG(ctx context.Context, sg SGSpec) error

	// UpdateSG replaces the SG's ACL set. Semantically identical to EnsureSG.
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

// applyACLs atomically replaces infra + tenant ACLs in one OVSDB transaction.
// ClearACLs+AddACLs split left zero-ACL gaps that defaulted traffic to drop.
func (m *sgManager) applyACLs(ctx context.Context, sg SGSpec) error {
	pg := topology.SecurityGroupPortGroup(sg.GroupID)
	specs := append(InfrastructureACLs(pg), RuleACLSpecs(pg, sg.IngressRules, sg.EgressRules)...)
	if err := m.ovn.ReplaceACLs(ctx, pg, specs); err != nil {
		return fmt.Errorf("replace ACLs on %s: %w", pg, err)
	}
	return nil
}
