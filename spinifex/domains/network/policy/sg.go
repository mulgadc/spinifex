package policy

import (
	"context"
	"fmt"
	"slices"

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
	ovn         ovn.Client
	egress      EgressPolicy
	prefixLists PrefixLists
}

var _ SecurityGroupManager = (*sgManager)(nil)

// SGOption configures a SecurityGroupManager.
type SGOption func(*sgManager)

// WithPrefixLists supplies the entries of the prefix lists a rule may name.
// Without it, a group with such a rule fails to apply.
func WithPrefixLists(lists PrefixLists) SGOption {
	return func(m *sgManager) { m.prefixLists = lists }
}

// NewSecurityGroupManager constructs a SecurityGroupManager backed by client.
// egress carries the platform WAN egress block applied to every guest.
func NewSecurityGroupManager(client ovn.Client, egress EgressPolicy, opts ...SGOption) SecurityGroupManager {
	m := &sgManager{ovn: client, egress: egress}
	for _, o := range opts {
		o(m)
	}
	return m
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
	// Written before the ACLs that name them, so no ACL ever names a set that
	// does not exist yet.
	if err := m.ensurePrefixListSets(ctx, sg); err != nil {
		return fmt.Errorf("ensure prefix list sets for %s: %w", pg, err)
	}
	if err := m.ovn.ReplaceACLs(ctx, pg, specs); err != nil {
		return fmt.Errorf("replace ACLs on %s: %w", pg, err)
	}
	return nil
}

// ensurePrefixListSets converges the address set of every prefix list sg's
// rules name to the list's current entries. A list with no entries gets an
// empty set, which matches nothing; an unknown list fails the group.
func (m *sgManager) ensurePrefixListSets(ctx context.Context, sg SGSpec) error {
	done := map[string]bool{}
	for _, r := range slices.Concat(sg.IngressRules, sg.EgressRules) {
		if r.PrefixListID == "" || done[r.PrefixListID] {
			continue
		}
		done[r.PrefixListID] = true
		name, err := PrefixListAddressSet(r.PrefixListID)
		if err != nil {
			return err
		}
		var cidrs []string
		ok := false
		if m.prefixLists != nil {
			cidrs, ok = m.prefixLists(r.PrefixListID)
		}
		if !ok {
			return fmt.Errorf("unknown prefix list %s", r.PrefixListID)
		}
		if cidrs == nil {
			cidrs = []string{}
		}
		if _, err := m.ovn.EnsureAddressSet(ctx, name, cidrs); err != nil {
			return fmt.Errorf("address set %s: %w", name, err)
		}
	}
	return nil
}
