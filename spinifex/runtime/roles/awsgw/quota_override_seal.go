package awsgw

import (
	"context"

	admissionv1 "github.com/mulgadc/spinifex/contracts/admission/v1"
	admissionquota "github.com/mulgadc/spinifex/spinifex/domains/admission/quota"
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
