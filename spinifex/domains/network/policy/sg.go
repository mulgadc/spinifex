package policy

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"

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
	// ConvergePrefixLists sets each known prefix list's address set to the
	// list's derived entries. Only the leader-gated reconcile pass calls it.
	ConvergePrefixLists(ctx context.Context) error
	// GrantingApplies counts EnsureSG calls in flight that started while
	// prefix-list peers were rendered.
	GrantingApplies() int
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
	listIDs     []string
	prefixLists PrefixLists
	granted     func() bool
	granting    atomic.Int64
}

var _ SecurityGroupManager = (*sgManager)(nil)

// SGOption configures a SecurityGroupManager.
type SGOption func(*sgManager)

// WithPrefixLists names the prefix lists a rule may name and derives their
// entries for ConvergePrefixLists. Without it, a group with such a rule fails.
func WithPrefixLists(lists PrefixLists, ids ...string) SGOption {
	return func(m *sgManager) { m.prefixLists, m.listIDs = lists, ids }
}

// WithPrefixListGrants reports whether prefix-list peers may grant. While it
// is false a rule naming a list renders no ACL and no set is written.
func WithPrefixListGrants(granted func() bool) SGOption {
	return func(m *sgManager) { m.granted = granted }
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
	// Counted before the grant is read, so a rollback that sees no apply in
	// flight is seen by every later apply. Decided once per apply.
	m.granting.Add(1)
	if !m.grants() {
		m.granting.Add(-1)
		sg.IngressRules = withoutPrefixLists(sg.IngressRules)
		sg.EgressRules = withoutPrefixLists(sg.EgressRules)
		return m.applyACLs(ctx, sg)
	}
	// Counted until it returns: an ACL it leaves after a withdrawal is seen by
	// the drain, whose poll wakes a pass that renders from current records.
	defer m.granting.Add(-1)
	return m.applyACLs(ctx, sg)
}

func (m *sgManager) grants() bool { return m.granted == nil || m.granted() }

func (m *sgManager) GrantingApplies() int { return int(m.granting.Load()) }

func withoutPrefixLists(rules []Rule) []Rule {
	return slices.DeleteFunc(slices.Clone(rules), func(r Rule) bool { return r.PrefixListID != "" })
}

// ConvergePrefixLists writes each known list's derived entries to its set,
// while prefix-list peers may grant. A list with no entries is an empty set.
func (m *sgManager) ConvergePrefixLists(ctx context.Context) error {
	if !m.grants() || m.prefixLists == nil {
		return nil
	}
	for _, id := range m.listIDs {
		name, err := PrefixListAddressSet(id)
		if err != nil {
			return err
		}
		cidrs, ok := m.prefixLists(id)
		if !ok {
			return fmt.Errorf("unknown prefix list %s", id)
		}
		if cidrs == nil {
			cidrs = []string{}
		}
		if _, err := m.ovn.EnsureAddressSet(ctx, name, cidrs); err != nil {
			return fmt.Errorf("converge address set %s: %w", name, err)
		}
	}
	return nil
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

// ensurePrefixListSets creates, empty, the set of every prefix list sg's rules
// name that is absent, and never touches an existing set's members; the
// reconcile pass fills it. An unknown list fails the group.
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
		if m.prefixLists == nil || !slices.Contains(m.listIDs, r.PrefixListID) {
			return fmt.Errorf("unknown prefix list %s", r.PrefixListID)
		}
		if _, err := m.ovn.CreateAddressSetIfAbsent(ctx, name); err != nil {
			return fmt.Errorf("address set %s: %w", name, err)
		}
	}
	return nil
}
