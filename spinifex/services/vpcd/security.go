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
	pgName := portGroupName(evt.GroupId)

	// Create port group (initially empty — ports are added when ENIs are assigned to the SG)
	if err := h.ovn.CreatePortGroup(ctx, pgName, nil); err != nil {
		slog.Error("vpcd: failed to create port group", "pg", pgName, "err", err)
		respond(msg, err)
		return
	}

	// Add default deny ACLs (priority 900) — drop all traffic not explicitly
	// allowed. Logging is enabled so boundary communications are observable via
	// syslog (CMMC SC.L1-3.13.1). Fail-fast: a partial ACL set leaves the port
	// group with the default-allow OVN behavior for any port that joins it.
	if err := h.ovn.AddACL(ctx, pgName, denyIngressACL(pgName)); err != nil {
		slog.Error("vpcd: failed to add default deny ingress ACL", "pg", pgName, "err", err)
		respond(msg, err)
		return
	}
	if err := h.ovn.AddACL(ctx, pgName, denyEgressACL(pgName)); err != nil {
		slog.Error("vpcd: failed to add default deny egress ACL", "pg", pgName, "err", err)
		respond(msg, err)
		return
	}

	// Add ACLs for initial rules (priority 1000 — higher than deny)
	if err := h.addRuleACLs(ctx, pgName, evt.IngressRules, evt.EgressRules); err != nil {
		respond(msg, err)
		return
	}

	slog.Info("vpcd: created security group port group",
		"pg", pgName,
		"group_id", evt.GroupId,
		"vpc_id", evt.VpcId,
		"ingress_rules", len(evt.IngressRules),
		"egress_rules", len(evt.EgressRules),
	)
	respond(msg, nil)
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

	// Clear all ACLs before deleting the port group
	if err := h.ovn.ClearACLs(ctx, pgName); err != nil {
		slog.Warn("vpcd: failed to clear ACLs", "pg", pgName, "err", err)
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

	// Re-add default deny ACLs (priority 900) with logging enabled. Fail-fast
	// — see handleCreateSG for rationale.
	if err := h.ovn.AddACL(ctx, pgName, denyIngressACL(pgName)); err != nil {
		slog.Error("vpcd: failed to re-add default deny ingress ACL", "pg", pgName, "err", err)
		respond(msg, err)
		return
	}
	if err := h.ovn.AddACL(ctx, pgName, denyEgressACL(pgName)); err != nil {
		slog.Error("vpcd: failed to re-add default deny egress ACL", "pg", pgName, "err", err)
		respond(msg, err)
		return
	}

	// Add ACLs for current rules
	if err := h.addRuleACLs(ctx, pgName, evt.IngressRules, evt.EgressRules); err != nil {
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

// addRuleACLs adds OVN ACLs for a set of ingress and egress rules at priority 1000
// (higher than the default deny at 900, so allow rules take precedence).
// Allow rules are not logged — accept logging on a private network is
// high-volume and low-signal. Only denies carry Log=true.
//
// Fail-fast on the first AddACL error: a partial allow set produces an
// insecure half-state once enforcement is on. The caller propagates via
// respond(msg, err) and the user retries; the reconciler is the safety net
// for crash recovery, not for transient ACL errors.
func (h *TopologyHandler) addRuleACLs(ctx context.Context, pgName string, ingress, egress []SGRuleForACL) error {
	for _, rule := range ingress {
		match := BuildIngressACLMatch(pgName, rule)
		spec := ACLSpec{Direction: "to-lport", Priority: 1000, Match: match, Action: "allow-related"}
		if err := h.ovn.AddACL(ctx, pgName, spec); err != nil {
			slog.Error("vpcd: failed to add ingress ACL", "pg", pgName, "match", match, "err", err)
			return err
		}
	}

	for _, rule := range egress {
		match := BuildEgressACLMatch(pgName, rule)
		spec := ACLSpec{Direction: "from-lport", Priority: 1000, Match: match, Action: "allow-related"}
		if err := h.ovn.AddACL(ctx, pgName, spec); err != nil {
			slog.Error("vpcd: failed to add egress ACL", "pg", pgName, "match", match, "err", err)
			return err
		}
	}
	return nil
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
