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

// provisionSG creates the OVN port group, address set, default-deny ACLs, and
// allow ACLs for the given SG. Used by handleCreateSG (CreateSecurityGroup
// path) and the reconciler's scan-2 (SG record without OVN port group).
//
// All-or-nothing: on any failure after the port group exists, the port group,
// address set, and any partial ACLs are torn down so the reconciler's scan-2
// can recreate the SG from scratch on its next pass. A partial state would
// otherwise look healthy to scanMissingPortGroups (port group exists) while
// silently dropping legitimate traffic via the default-deny ACL.
func (h *TopologyHandler) provisionSG(ctx context.Context, groupId string, ingress, egress []SGRuleForACL) error {
	pgName := portGroupName(groupId)
	asName := addressSetName(pgName)

	// Create port group (initially empty — ports are added when ENIs are assigned to the SG)
	if err := h.ovn.CreatePortGroup(ctx, pgName, nil); err != nil {
		return fmt.Errorf("create port group %s: %w", pgName, err)
	}

	done := false
	defer func() {
		if done {
			return
		}
		// Best-effort rollback of the half-built SG. Each step logs but does
		// not propagate — we already have the original error to return, and
		// the reconciler's orphan-PG scan is the safety net for any leftover.
		if err := h.ovn.ClearACLs(ctx, pgName); err != nil {
			slog.Warn("vpcd: provisionSG cleanup ClearACLs failed", "pg", pgName, "err", err)
		}
		if err := h.ovn.DeletePortGroup(ctx, pgName); err != nil {
			slog.Warn("vpcd: provisionSG cleanup DeletePortGroup failed", "pg", pgName, "err", err)
		}
		if err := h.ovn.DeleteAddressSet(ctx, asName); err != nil {
			slog.Warn("vpcd: provisionSG cleanup DeleteAddressSet failed", "as", asName, "err", err)
		}
	}()

	// Create the per-SG address set. ACLs whose match expression references
	// this SG as a SourceSG (e.g. "ip4.src == $<asName>") need the set to
	// resolve, otherwise libovsdb errors at ACL evaluation time. Empty until
	// ports join via reconcilePortSGs.
	if err := h.ovn.CreateAddressSet(ctx, asName, nil); err != nil {
		return fmt.Errorf("create address set %s: %w", asName, err)
	}

	// Default deny ACLs (priority 900, logged for CMMC SC.L1-3.13.1) and
	// allow rules (priority 1000) go in one OVSDB transaction so a 60-rule
	// SG creation is one round-trip, not 62.
	specs := append([]ACLSpec{denyIngressACL(pgName), denyEgressACL(pgName)}, ruleACLSpecs(pgName, ingress, egress)...)
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
	asName := addressSetName(pgName)

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

	// Once the port group is gone the orphan-PG reconciler scan can no longer
	// anchor cleanup of the matching address set, so a swallowed error here
	// would leak the AS forever. Fail-fast and let the caller retry.
	if err := h.ovn.DeleteAddressSet(ctx, asName); err != nil {
		slog.Error("vpcd: failed to delete address set", "as", asName, "err", err)
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

	// Re-add default deny ACLs + current rules in one transaction.
	specs := append([]ACLSpec{denyIngressACL(pgName), denyEgressACL(pgName)}, ruleACLSpecs(pgName, evt.IngressRules, evt.EgressRules)...)
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
