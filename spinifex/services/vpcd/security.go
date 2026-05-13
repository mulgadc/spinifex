package vpcd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go"
)

// NATS topics for security group lifecycle events.
const (
	TopicCreateSG = "vpc.create-sg"
	TopicDeleteSG = "vpc.delete-sg"
	TopicUpdateSG = "vpc.update-sg"
)

// denyACLSeverity is the syslog severity OVN uses when a packet matches a
// default-deny ACL. "info" is loud enough to be captured by a syslog forwarder
// yet quiet enough to avoid paging on every dropped scan packet. Operators can
// promote it at their log collector if they want higher-priority alerts.
const denyACLSeverity = "info"

// SGEvent carries security group state from the handler to vpcd.
type SGEvent struct {
	GroupId      string         `json:"group_id"`
	VpcId        string         `json:"vpc_id"`
	IngressRules []SGRuleForACL `json:"ingress_rules,omitempty"`
	EgressRules  []SGRuleForACL `json:"egress_rules,omitempty"`
}

// handleCreateSG creates an OVN Port Group and initial ACLs for a new security group.
func (h *TopologyHandler) handleCreateSG(msg *nats.Msg) {
	if h.ovn == nil {
		respond(msg, fmt.Errorf("OVN client not connected"))
		return
	}

	var evt SGEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("vpcd: failed to unmarshal vpc.create-sg event", "err", err)
		respond(msg, err)
		return
	}

	ctx := context.Background()
	if err := h.provisionSG(ctx, evt.GroupId, evt.IngressRules, evt.EgressRules); err != nil {
		slog.Error("vpcd: failed to provision security group", "group_id", evt.GroupId, "err", err)
		respond(msg, err)
		return
	}

	slog.Info("vpcd: created security group port group",
		"pg", portGroupName(evt.GroupId),
		"group_id", evt.GroupId,
		"vpc_id", evt.VpcId,
		"ingress_rules", len(evt.IngressRules),
		"egress_rules", len(evt.EgressRules),
	)
	respond(msg, nil)
}

// provisionSG creates the OVN port group, default-deny ACLs, and allow ACLs
// for the given SG. Used by handleCreateSG (CreateSecurityGroup path) and the
// reconciler's scan-2 (SG record without OVN port group).
//
// SG-to-SG match expressions reference `$<pg>_ip4` / `$<pg>_ip6` — these
// resolve against the SB Address_Set rows that ovn-northd auto-derives from
// each port group's port addresses (ovn-nb(5): "For each port group, there
// are two address sets generated to the Address_Set table of the
// OVN_Southbound database, ... with name being the name of the Port_Group
// followed by a suffix _ip4 / _ip6"). vpcd must NOT create explicit NB
// Address_Set rows with those names; that produces a duplicate sync to SB
// and wedges northd in an `OVNSB commit failed` retry loop.
//
// All-or-nothing: on any failure after the port group exists, the port group
// and any partial ACLs are torn down so the reconciler's scan-2 can recreate
// the SG from scratch on its next pass. A partial state would otherwise look
// healthy to scanMissingPortGroups (port group exists) while silently
// dropping legitimate traffic via the default-deny ACL.
func (h *TopologyHandler) provisionSG(ctx context.Context, groupId string, ingress, egress []SGRuleForACL) error {
	pgName := portGroupName(groupId)

	if err := h.ovn.CreatePortGroup(ctx, pgName, nil); err != nil {
		return fmt.Errorf("create port group %s: %w", pgName, err)
	}

	done := false
	defer func() {
		if done {
			return
		}
		if err := h.ovn.ClearACLs(ctx, pgName); err != nil {
			slog.Warn("vpcd: provisionSG cleanup ClearACLs failed", "pg", pgName, "err", err)
		}
		if err := h.ovn.DeletePortGroup(ctx, pgName); err != nil {
			slog.Warn("vpcd: provisionSG cleanup DeletePortGroup failed", "pg", pgName, "err", err)
		}
	}()

	// Default deny ACLs (priority 900, logged for CMMC SC.L1-3.13.1),
	// infrastructure DHCP allows (priority 1050), and tenant allow rules
	// (priority 1000) go in one OVSDB transaction so a 60-rule SG creation
	// is one round-trip, not 64.
	specs := append(infrastructureACLs(pgName), ruleACLSpecs(pgName, ingress, egress)...)
	if err := h.ovn.AddACLs(ctx, pgName, specs); err != nil {
		return fmt.Errorf("add ACLs on %s: %w", pgName, err)
	}
	done = true
	return nil
}

// handleDeleteSG deletes the OVN Port Group and all associated ACLs.
func (h *TopologyHandler) handleDeleteSG(msg *nats.Msg) {
	if h.ovn == nil {
		respond(msg, fmt.Errorf("OVN client not connected"))
		return
	}

	var evt SGEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("vpcd: failed to unmarshal vpc.delete-sg event", "err", err)
		respond(msg, err)
		return
	}

	ctx := context.Background()
	pgName := portGroupName(evt.GroupId)

	// Clear all ACLs before deleting the port group. Fail-fast — leaving stale
	// ACLs causes DeletePortGroup to be rejected by libovsdb (dangling ref).
	if err := h.ovn.ClearACLs(ctx, pgName); err != nil {
		slog.Error("vpcd: failed to clear ACLs", "pg", pgName, "err", err)
		respond(msg, err)
		return
	}

	if err := h.ovn.DeletePortGroup(ctx, pgName); err != nil {
		slog.Error("vpcd: failed to delete port group", "pg", pgName, "err", err)
		respond(msg, err)
		return
	}

	slog.Info("vpcd: deleted security group port group",
		"pg", pgName,
		"group_id", evt.GroupId,
		"vpc_id", evt.VpcId,
	)
	respond(msg, nil)
}

// handleUpdateSG replaces all ACLs for a security group with the current rule set.
func (h *TopologyHandler) handleUpdateSG(msg *nats.Msg) {
	if h.ovn == nil {
		respond(msg, fmt.Errorf("OVN client not connected"))
		return
	}

	var evt SGEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("vpcd: failed to unmarshal vpc.update-sg event", "err", err)
		respond(msg, err)
		return
	}

	ctx := context.Background()
	pgName := portGroupName(evt.GroupId)

	// Clear existing ACLs
	if err := h.ovn.ClearACLs(ctx, pgName); err != nil {
		slog.Error("vpcd: failed to clear ACLs for update", "pg", pgName, "err", err)
		respond(msg, err)
		return
	}

	// Re-add default deny + DHCP-infra allow ACLs + current rules in one transaction.
	specs := append(infrastructureACLs(pgName), ruleACLSpecs(pgName, evt.IngressRules, evt.EgressRules)...)
	if err := h.ovn.AddACLs(ctx, pgName, specs); err != nil {
		slog.Error("vpcd: failed to add ACLs for update", "pg", pgName, "err", err)
		respond(msg, err)
		return
	}

	slog.Info("vpcd: updated security group ACLs",
		"pg", pgName,
		"group_id", evt.GroupId,
		"vpc_id", evt.VpcId,
		"ingress_rules", len(evt.IngressRules),
		"egress_rules", len(evt.EgressRules),
	)
	respond(msg, nil)
}

// ruleACLSpecs builds the priority-1000 allow ACL specs for the given rules.
// Allow rules are not logged — accept logging on a private network is high
// volume and low signal; only denies carry Log=true.
func ruleACLSpecs(pgName string, ingress, egress []SGRuleForACL) []ACLSpec {
	specs := make([]ACLSpec, 0, len(ingress)+len(egress))
	for _, rule := range ingress {
		specs = append(specs, ACLSpec{Direction: "to-lport", Priority: 1000, Match: BuildIngressACLMatch(pgName, rule), Action: "allow-related"})
	}
	for _, rule := range egress {
		specs = append(specs, ACLSpec{Direction: "from-lport", Priority: 1000, Match: BuildEgressACLMatch(pgName, rule), Action: "allow-related"})
	}
	return specs
}

// denyIngressACL builds the default-deny ingress ACL for a port group with
// logging enabled (CMMC SC.L1-3.13.1 boundary monitoring).
func denyIngressACL(pgName string) ACLSpec {
	return ACLSpec{
		Direction: "to-lport",
		Priority:  900,
		Match:     fmt.Sprintf("outport == @%s && ip4", pgName),
		Action:    "drop",
		Name:      pgName + "-deny-ingress",
		Log:       true,
		Severity:  denyACLSeverity,
	}
}

// denyEgressACL is the egress counterpart to denyIngressACL.
func denyEgressACL(pgName string) ACLSpec {
	return ACLSpec{
		Direction: "from-lport",
		Priority:  900,
		Match:     fmt.Sprintf("inport == @%s && ip4", pgName),
		Action:    "drop",
		Name:      pgName + "-deny-egress",
		Log:       true,
		Severity:  denyACLSeverity,
	}
}

// infrastructureACLs returns the platform-level ACLs that every port group
// carries regardless of tenant rules: the priority-900 default-deny pair and
// the priority-1050 DHCPv4 client/server allow pair. OVN's built-in DHCP
// responder runs inside the LS pipeline *after* the port-group ACL stage, so
// without these allows a narrow-egress tenant SG (e.g. egress restricted to
// the VPC CIDR) silently drops the guest's DHCPDISCOVER (dst=255.255.255.255)
// against the default-deny and the VM never gets an IPv4 lease. AWS users
// can't write a rule for the limited broadcast address, so the platform must
// allow DHCP itself — matching AWS's implicit behaviour.
//
// Priority 1050 sits above the default-deny (900) and above tenant rules
// (1000) so DHCP works regardless of tenant configuration; tenants in our
// model don't write ACLs at >1000, so there is no collision.
func infrastructureACLs(pgName string) []ACLSpec {
	return []ACLSpec{
		denyIngressACL(pgName),
		denyEgressACL(pgName),
		dhcpEgressACL(pgName),
		dhcpIngressACL(pgName),
	}
}

// dhcpEgressACL allows the guest's DHCP client traffic (DHCPDISCOVER,
// DHCPREQUEST: udp.src=68, udp.dst=67) out of the port. Action is plain
// "allow", not "allow-related" — DHCP is single-shot request/reply and
// allow-related on broadcast UDP interacts oddly with OVN's CT zones.
func dhcpEgressACL(pgName string) ACLSpec {
	return ACLSpec{
		Direction: "from-lport",
		Priority:  1050,
		Match:     fmt.Sprintf("inport == @%s && udp && udp.src == 68 && udp.dst == 67", pgName),
		Action:    "allow",
		Name:      pgName + "-allow-dhcp-egress",
	}
}

// dhcpIngressACL allows the OVN-generated DHCP server reply (DHCPOFFER,
// DHCPACK: udp.src=67, udp.dst=68) back to the guest. OVN's DHCP responder
// emits the reply from inside the LS pipeline, so without this matching
// to-lport allow the reply hits the priority-900 ingress default-deny.
func dhcpIngressACL(pgName string) ACLSpec {
	return ACLSpec{
		Direction: "to-lport",
		Priority:  1050,
		Match:     fmt.Sprintf("outport == @%s && udp && udp.src == 67 && udp.dst == 68", pgName),
		Action:    "allow",
		Name:      pgName + "-allow-dhcp-ingress",
	}
}
