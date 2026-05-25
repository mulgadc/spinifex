package subscribers

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/nats-io/nats.go"
)

func (e SGEvent) toSpec() policy.SGSpec {
	return policy.SGSpec{
		GroupID:      e.GroupId,
		VPCID:        e.VpcId,
		IngressRules: toPolicyRules(e.IngressRules),
		EgressRules:  toPolicyRules(e.EgressRules),
	}
}

func toPolicyRules(in []SGRule) []policy.Rule {
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
func (s *Subscriber) handleCreateSG(msg *nats.Msg) {
	var evt SGEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("subscribers: failed to unmarshal vpc.create-sg event", "err", err)
		respond(msg, err)
		return
	}
	ctx := context.Background()
	if err := s.topology.EnsureSGPortGroup(ctx, evt.GroupId); err != nil {
		slog.Error("subscribers: EnsureSGPortGroup failed", "group_id", evt.GroupId, "err", err)
		respond(msg, err)
		return
	}
	if err := s.sg.EnsureSG(ctx, evt.toSpec()); err != nil {
		slog.Error("subscribers: EnsureSG failed", "group_id", evt.GroupId, "err", err)
		respond(msg, err)
		return
	}
	slog.Info("subscribers: created security group",
		"group_id", evt.GroupId,
		"vpc_id", evt.VpcId,
		"ingress_rules", len(evt.IngressRules),
		"egress_rules", len(evt.EgressRules),
	)
	respond(msg, nil)
}

// handleDeleteSG removes the SG's port group (and its ACLs in one OVN
// transaction). Idempotent on already-absent.
func (s *Subscriber) handleDeleteSG(msg *nats.Msg) {
	var evt SGEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("subscribers: failed to unmarshal vpc.delete-sg event", "err", err)
		respond(msg, err)
		return
	}
	if err := s.topology.DeleteSGPortGroup(context.Background(), evt.GroupId); err != nil {
		slog.Error("subscribers: DeleteSGPortGroup failed", "group_id", evt.GroupId, "err", err)
		respond(msg, err)
		return
	}
	slog.Info("subscribers: deleted security group", "group_id", evt.GroupId, "vpc_id", evt.VpcId)
	respond(msg, nil)
}

// handleUpdateSG replaces the SG's ACL set with the rules from the event.
// Port-group lifecycle is unaffected — UpdateSG only touches ACLs.
func (s *Subscriber) handleUpdateSG(msg *nats.Msg) {
	var evt SGEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("subscribers: failed to unmarshal vpc.update-sg event", "err", err)
		respond(msg, err)
		return
	}
	if err := s.sg.UpdateSG(context.Background(), evt.toSpec()); err != nil {
		slog.Error("subscribers: UpdateSG failed", "group_id", evt.GroupId, "err", err)
		respond(msg, err)
		return
	}
	slog.Info("subscribers: updated security group",
		"group_id", evt.GroupId,
		"vpc_id", evt.VpcId,
		"ingress_rules", len(evt.IngressRules),
		"egress_rules", len(evt.EgressRules),
	)
	respond(msg, nil)
}
