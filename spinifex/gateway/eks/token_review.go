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

// tokenReviewVerifyTimeout bounds the host-side STS verify NATS round-trip.
const tokenReviewVerifyTimeout = 5 * time.Second

// webhookTokenReviewRequest is the body POSTed to /clusters/{name}/token-review.
// The webhook uses system SigV4 creds, so AccountID names the cluster account explicitly.
type webhookTokenReviewRequest struct {
	AccountID string `json:"accountId"`
	Token     string `json:"token"`
}

// WebhookTokenReview — POST /clusters/{name}/token-review. Resolves an
// `aws eks get-token` bearer token to a K8s identity (STS verify + AccessEntry
// lookup) via the AWSGW, keeping STS/KV access cluster-internal.
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
