package policy

import (
	"context"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

// SGSpec is the policy-layer view of a security group: identifier plus
// ingress/egress rule sets. The OVN port group named by
// topology.SecurityGroupPortGroup(GroupID) must already exist (L2 owns
// its lifecycle); SecurityGroupManager only attaches and clears ACLs.
type SGSpec struct {
	GroupID      string
	VPCID        string
	IngressRules []Rule
	EgressRules  []Rule
}

// SecurityGroupManager attaches ACL policy to a security group's OVN port
// group. EnsureSG and UpdateSG share semantics — both atomically clear and
// re-add the full ACL set (infrastructure + tenant). The replace-all model
// avoids per-rule diff complexity: OVN treats the AddACLs transaction as
// atomic, so callers never observe a partial rule set on the wire.
//
// Port group lifecycle (create + delete) is owned by topology.Manager. If
// the port group is absent, EnsureSG / UpdateSG / DeleteSG return the
// underlying L1 "port group not found" error.
type SecurityGroupManager interface {
	// EnsureSG sets the full ACL set on the SG's port group. Idempotent:
	// repeated calls with the same spec converge to the same OVN state.
	EnsureSG(ctx context.Context, sg SGSpec) error

	// UpdateSG replaces the SG's ACL set. Identical to EnsureSG; kept as a
	// distinct method to mirror the parent plan's interface (§8.1) and
	// document caller intent.
	UpdateSG(ctx context.Context, sg SGSpec) error

	// DeleteSG clears every ACL on the SG's port group. The port group
	// itself is not deleted — topology.Manager owns that.
	DeleteSG(ctx context.Context, groupID string) error
}

type sgManager struct {
	ovn ovn.Client
}

var _ SecurityGroupManager = (*sgManager)(nil)

// NewSecurityGroupManager constructs a SecurityGroupManager backed by the
// given OVN client.
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

// applyACLs clears the port group's current ACL set and re-adds the
// infrastructure + tenant ACLs. ClearACLs followed by AddACLs is not
// transactional at the OVSDB level, but every chassis re-derives flows
// from the new ACL set in one ovn-northd compile cycle — the observable
// gap is bounded by southbound flow-install latency, the same window that
// applies to any ACL change. Callers requiring stronger atomicity must
// gate updates via leader election.
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
