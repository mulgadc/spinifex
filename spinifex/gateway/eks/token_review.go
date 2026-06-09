package gateway_eks

import (
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/nats-io/nats.go"
)

// tokenReviewVerifyTimeout bounds the host-side STS verify NATS round-trip. The
// apiserver fails the TokenReview on its own timeout, so keep this tight.
const tokenReviewVerifyTimeout = 5 * time.Second

// webhookTokenReviewRequest is the body the on-VM eks-token-webhook POSTs to
// /clusters/{name}/token-review. The webhook holds system SigV4 creds (system
// account, not the cluster account), so AccountID names the cluster account
// explicitly — same pattern as PublishInternal.
type webhookTokenReviewRequest struct {
	AccountID string `json:"accountId"`
	Token     string `json:"token"`
}

// WebhookTokenReview — POST /clusters/{name}/token-review. Resolves an
// `aws eks get-token` bearer token to a K8s identity host-side (STS verify +
// AccessEntry lookup) and returns it. This is the gateway-broker replacement
// for the webhook dialing core NATS/STS/KV directly.
func WebhookTokenReview(natsConn *nats.Conn, clusterName string, body []byte) (*handlers_eks.WebhookTokenReviewResult, error) {
	if natsConn == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if clusterName == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	var req webhookTokenReviewRequest
	if err := json.Unmarshal(body, &req); err != nil {
		slog.Debug("WebhookTokenReview: bad body", "cluster", clusterName, "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if req.AccountID == "" || req.Token == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	res, err := handlers_eks.ResolveTokenReview(natsConn, req.AccountID, clusterName, req.Token, tokenReviewVerifyTimeout)
	if err != nil {
		slog.Error("WebhookTokenReview: resolve failed", "cluster", clusterName, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	return &res, nil
}
