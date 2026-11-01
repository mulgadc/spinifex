package awsgw

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	admissionv1 "github.com/mulgadc/spinifex/contracts/admission/v1"
	admissionquota "github.com/mulgadc/spinifex/spinifex/domains/admission/quota"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
)

// quotaOverrideSealQueue is the NATS queue group for the quota override seal
// responders so the request is load-balanced across awsgw nodes.
const quotaOverrideSealQueue = "awsgw-quota-seal"

// registerQuotaOverrideSeal serves SealAccountQuotaOverrideSubject on this
// gateway. Any current gateway may answer; the leader-accepted revision is
// the evidence.
func registerQuotaOverrideSeal(nc *nats.Conn, quota *admissionquota.Service) (*nats.Subscription, error) {
	return nc.QueueSubscribe(admissionv1.SealAccountQuotaOverrideSubject, quotaOverrideSealQueue,
		func(msg *nats.Msg) {
			natsmsg.ServeNATSRequestCtx(msg, handleSealAccountQuotaOverride(quota))
		})
}

func handleSealAccountQuotaOverride(quota *admissionquota.Service) func(context.Context, *admissionv1.SealAccountQuotaOverrideRequest) (*admissionv1.SealAccountQuotaOverrideReply, error) {
	return func(ctx context.Context, req *admissionv1.SealAccountQuotaOverrideRequest) (*admissionv1.SealAccountQuotaOverrideReply, error) {
		seal, err := quota.SealAccountOverrides(ctx, req.AccountID)
		if err != nil {
			return nil, err
		}
		return &admissionv1.SealAccountQuotaOverrideReply{
			AccountID:      req.AccountID,
			CorrelationRef: req.CorrelationRef,
			Disposition:    admissionv1.AccountQuotaOverrideDispositionSealed,
			Revision:       seal.Revision,
		}, nil
	}
}

// registerQuotaOverrideSealCapability serves this gateway's read-only seal
// capability on its own node's subject, with no queue group so a probe reaches
// every gateway configured as that node. An invalid node ID serves nothing and
// returns a nil subscription, so the probe reads this gateway as incompatible.
func registerQuotaOverrideSealCapability(nc *nats.Conn, node string) (*nats.Subscription, error) {
	if !admissionv1.ValidSealCapabilityNode(node) {
		slog.Warn("Quota override seal: node ID is not a valid subject token, capability not served", "node", node)
		return nil, nil
	}
	return nc.Subscribe(admissionv1.AccountQuotaOverrideSealCapabilitySubject(node), func(msg *nats.Msg) {
		respondWithNodeID(node, msg, quotaOverrideSealCapabilityReply(node, msg.Data))
	})
}

// quotaOverrideSealCapabilityReply answers one capability request: its own
// node with version 1, or an error payload for a malformed request or a
// request naming another node. It reads no KV and takes no account.
func quotaOverrideSealCapabilityReply(node string, data []byte) []byte {
	var req admissionv1.AccountQuotaOverrideSealCapabilityRequest
	if errResp := awserrors.UnmarshalJsonPayload(&req, data); errResp != nil {
		return errResp
	}
	if req.Node != node {
		return awserrors.GenerateErrorPayloadWithMessage(awserrors.ErrorInvalidParameterValue,
			fmt.Sprintf("seal capability request for node %q answered by node %q", req.Node, node))
	}
	out, err := json.Marshal(admissionv1.AccountQuotaOverrideSealCapabilityReply{
		Node:              node,
		CapabilityVersion: admissionv1.AccountQuotaOverrideSealCapabilityVersion,
		CorrelationRef:    req.CorrelationRef,
	})
	if err != nil {
		return awserrors.GenerateErrorPayload(awserrors.ErrorServerInternal)
	}
	return out
}

// respondWithNodeID replies with the X-Node-ID header the probe uses to tell
// which gateway answered; natsmsg.ServeNATSRequestCtx sets none.
func respondWithNodeID(node string, msg *nats.Msg, data []byte) {
	reply := nats.NewMsg("")
	reply.Data = data
	reply.Header.Set(natsmsg.NodeIDHeader, node)
	if err := msg.RespondMsg(reply); err != nil {
		slog.Error("Quota override seal: failed to respond to capability request", "err", err)
	}
}
