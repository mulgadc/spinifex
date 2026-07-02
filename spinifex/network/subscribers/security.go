package subscribers

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/nats-io/nats.go"
)

// sgHandlerTimeout bounds each SG handler's OVN work so a stuck NB op frees the
// worker instead of hanging on context.Background forever. Sits above worst-case
// OVN write under raft lag, well under any pathological hang.
const sgHandlerTimeout = 10 * time.Second

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

// handleCreateSG ensures the PG (L2) then applies the ACL set (L3).
func (s *Subscriber) handleCreateSG(msg *nats.Msg) {
	var evt SGEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("subscribers: failed to unmarshal vpc.create-sg event", "err", err)
		respond(msg, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sgHandlerTimeout)
	defer cancel()
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

// handleDeleteSG removes the SG's PG and its ACLs; idempotent.
func (s *Subscriber) handleDeleteSG(msg *nats.Msg) {
	var evt SGEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("subscribers: failed to unmarshal vpc.delete-sg event", "err", err)
		respond(msg, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sgHandlerTimeout)
	defer cancel()
	if err := s.topology.DeleteSGPortGroup(ctx, evt.GroupId); err != nil {
		slog.Error("subscribers: DeleteSGPortGroup failed", "group_id", evt.GroupId, "err", err)
		respond(msg, err)
		return
	}
	slog.Info("subscribers: deleted security group", "group_id", evt.GroupId, "vpc_id", evt.VpcId)
	respond(msg, nil)
}

// handleUpdateSG replaces the ACL set; the PG is unaffected.
func (s *Subscriber) handleUpdateSG(msg *nats.Msg) {
	var evt SGEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("subscribers: failed to unmarshal vpc.update-sg event", "err", err)
		respond(msg, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sgHandlerTimeout)
	defer cancel()
	if err := s.sg.UpdateSG(ctx, evt.toSpec()); err != nil {
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
