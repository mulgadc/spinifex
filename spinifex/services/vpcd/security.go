package vpcd

// Phase 2.6 (mulga-siv-129) thin NATS wrappers for SG lifecycle. Each handler
// deserialises the event, translates to the policy/topology layer's spec
// types, and calls the manager. ACL semantics, OVN port-group lifecycle and
// the full all-or-nothing convergence model live in network/topology and
// network/policy; this file owns NATS plumbing only.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/nats-io/nats.go"
)

// sgRule mirrors the on-wire payload from handlers/ec2/vpc.SGRule. Kept local
// so vpcd does not import the handlers package.
type sgRule struct {
	IpProtocol string `json:"ip_protocol"`
	FromPort   int64  `json:"from_port"`
	ToPort     int64  `json:"to_port"`
	CidrIp     string `json:"cidr_ip,omitempty"`
	SourceSG   string `json:"source_sg,omitempty"`
}

// sgEvent mirrors handlers/ec2/vpc.SGEvent.
type sgEvent struct {
	GroupId      string   `json:"group_id"`
	VpcId        string   `json:"vpc_id"`
	IngressRules []sgRule `json:"ingress_rules,omitempty"`
	EgressRules  []sgRule `json:"egress_rules,omitempty"`
}

func (e sgEvent) toSpec() policy.SGSpec {
	return policy.SGSpec{
		GroupID:      e.GroupId,
		VPCID:        e.VpcId,
		IngressRules: toPolicyRules(e.IngressRules),
		EgressRules:  toPolicyRules(e.EgressRules),
	}
}

func toPolicyRules(in []sgRule) []policy.Rule {
	out := make([]policy.Rule, len(in))
	for i, r := range in {
		out[i] = policy.Rule{
			IPProtocol: r.IpProtocol,
			FromPort:   r.FromPort,
			ToPort:     r.ToPort,
			CIDR:       r.CidrIp,
			SourceSG:   r.SourceSG,
		}
	}
	return out
}

// handleCreateSG ensures the OVN port group exists then applies the ACL set
// for a new security group. Two-step (topology + policy) because port-group
// lifecycle is L2 and ACL programming is L3 — see plan §8.1.
func (h *TopologyHandler) handleCreateSG(msg *nats.Msg) {
	if h.ovn == nil {
		respond(msg, fmt.Errorf("OVN client not connected"))
		return
	}
	var evt sgEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("vpcd: failed to unmarshal vpc.create-sg event", "err", err)
		respond(msg, err)
		return
	}
	ctx := context.Background()
	if err := h.EnsureSGPortGroup(ctx, evt.GroupId); err != nil {
		slog.Error("vpcd: EnsureSGPortGroup failed", "group_id", evt.GroupId, "err", err)
		respond(msg, err)
		return
	}
	if err := h.securityGroupManager().EnsureSG(ctx, evt.toSpec()); err != nil {
		slog.Error("vpcd: EnsureSG failed", "group_id", evt.GroupId, "err", err)
		respond(msg, err)
		return
	}
	slog.Info("vpcd: created security group",
		"group_id", evt.GroupId,
		"vpc_id", evt.VpcId,
		"ingress_rules", len(evt.IngressRules),
		"egress_rules", len(evt.EgressRules),
	)
	respond(msg, nil)
}

// handleDeleteSG removes the SG's port group (and its ACLs in one OVN
// transaction). Idempotent on already-absent.
func (h *TopologyHandler) handleDeleteSG(msg *nats.Msg) {
	if h.ovn == nil {
		respond(msg, fmt.Errorf("OVN client not connected"))
		return
	}
	var evt sgEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("vpcd: failed to unmarshal vpc.delete-sg event", "err", err)
		respond(msg, err)
		return
	}
	ctx := context.Background()
	if err := h.DeleteSGPortGroup(ctx, evt.GroupId); err != nil {
		slog.Error("vpcd: DeleteSGPortGroup failed", "group_id", evt.GroupId, "err", err)
		respond(msg, err)
		return
	}
	slog.Info("vpcd: deleted security group", "group_id", evt.GroupId, "vpc_id", evt.VpcId)
	respond(msg, nil)
}

// handleUpdateSG replaces the SG's ACL set with the rules from the event.
// Port-group lifecycle is unaffected — UpdateSG only touches ACLs.
func (h *TopologyHandler) handleUpdateSG(msg *nats.Msg) {
	if h.ovn == nil {
		respond(msg, fmt.Errorf("OVN client not connected"))
		return
	}
	var evt sgEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("vpcd: failed to unmarshal vpc.update-sg event", "err", err)
		respond(msg, err)
		return
	}
	ctx := context.Background()
	if err := h.securityGroupManager().UpdateSG(ctx, evt.toSpec()); err != nil {
		slog.Error("vpcd: UpdateSG failed", "group_id", evt.GroupId, "err", err)
		respond(msg, err)
		return
	}
	slog.Info("vpcd: updated security group",
		"group_id", evt.GroupId,
		"vpc_id", evt.VpcId,
		"ingress_rules", len(evt.IngressRules),
		"egress_rules", len(evt.EgressRules),
	)
	respond(msg, nil)
}
