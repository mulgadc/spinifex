package gateway_eks

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/nats-io/nats.go"
)

// tokenReviewVerifyTimeout bounds the host-side STS verify NATS round-trip.
const tokenReviewVerifyTimeout = 5 * time.Second

// webhookTokenReviewRequest is the body POSTed to /clusters/{name}/token-review.
// The webhook signs as the CP VM's system-account instance role, so AccountID
// names the cluster account explicitly.
type webhookTokenReviewRequest struct {
	AccountID string `json:"accountId"`
	Token     string `json:"token"`
}

// decodeWebhookTokenReview is the one reading of the body that both
// AuthorizeInternal and WebhookTokenReview use, so the account the gate binds
// is the account whose access entries the review reads.
func decodeWebhookTokenReview(body []byte) (webhookTokenReviewRequest, error) {
	var req webhookTokenReviewRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return webhookTokenReviewRequest{}, err
	}
	if req.AccountID == "" {
		return webhookTokenReviewRequest{}, errors.New("accountId is required")
	}
	return req, nil
}

// WebhookTokenReview — POST /clusters/{name}/token-review. Resolves an
// `aws eks get-token` bearer token to a K8s identity via the AWSGW, once
// AuthorizeInternal has bound the caller to this cluster and account.
func WebhookTokenReview(ctx context.Context, natsConn *nats.Conn, clusterName string, body []byte) (*handlers_eks.WebhookTokenReviewResult, error) {
	if natsConn == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if clusterName == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	req, err := decodeWebhookTokenReview(body)
	if err != nil {
		slog.DebugContext(ctx, "WebhookTokenReview: bad body", "cluster", clusterName, "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if req.Token == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	res, err := handlers_eks.ResolveTokenReview(ctx, natsConn, req.AccountID, clusterName, req.Token, tokenReviewVerifyTimeout)
	if err != nil {
		slog.ErrorContext(ctx, "WebhookTokenReview: resolve failed", "cluster", clusterName, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	return &res, nil
}
