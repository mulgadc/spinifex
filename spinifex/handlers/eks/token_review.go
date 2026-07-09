package handlers_eks

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// WebhookTokenReviewResult is the gateway's reply to the eks-token-webhook's
// TokenReview relay: the resolved K8s identity for an `aws eks get-token`
// bearer token, or Authenticated=false. The webhook maps it into the
// authentication.k8s.io/v1 TokenReview the apiserver expects. The locationName
// tags let the gateway's restjson marshaler (jsonutil) emit the same keys the
// json tags name, so the webhook decodes the body with encoding/json.
type WebhookTokenReviewResult struct {
	Authenticated bool     `json:"authenticated" locationName:"authenticated"`
	Username      string   `json:"username,omitempty" locationName:"username"`
	UID           string   `json:"uid,omitempty" locationName:"uid"`
	Groups        []string `json:"groups,omitempty" locationName:"groups"`
}

// Authenticate is the pure TokenReview decision core: decode token → STS verify
// → AccessEntry lookup → identity. Any failure yields Authenticated=false (never
// an error) so a forged/unknown token is a clean K8s 401, not a webhook outage.
// verify and lookup are injected so the logic is unit-testable without NATS.
func Authenticate(
	token string,
	verify func(presignedURL string) (*TokenVerifyResponse, error),
	lookup func(principalARN string) (*AccessEntryRecord, error),
) WebhookTokenReviewResult {
	presignedURL, err := DecodeGetToken(token)
	if err != nil {
		return WebhookTokenReviewResult{Authenticated: false}
	}
	ident, err := verify(presignedURL)
	if err != nil {
		slog.Debug("token verify rejected", "err", err)
		return WebhookTokenReviewResult{Authenticated: false}
	}
	rec, err := lookup(ident.ARN)
	if err != nil {
		// No AccessEntry for an otherwise-valid IAM principal: authenticated to
		// AWS, but not granted any K8s identity on this cluster.
		slog.Debug("no access entry for principal", "arn", ident.ARN, "err", err)
		return WebhookTokenReviewResult{Authenticated: false}
	}
	uid := ident.UserID
	if uid == "" {
		uid = ident.ARN
	}
	return WebhookTokenReviewResult{
		Authenticated: true,
		Username:      rec.KubernetesUsername,
		UID:           uid,
		Groups:        rec.KubernetesGroups,
	}
}

// ResolveTokenReview runs the TokenReview decision host-side, wiring the real
// STS verify (NATS request to the awsgw-hosted verify subject) and AccessEntry
// KV read into Authenticate. The eks-token-webhook reaches this through the
// gateway broker (POST /clusters/{name}/token-review) so the control-plane VM
// never speaks NATS. Returns an error only for a genuine infrastructure fault
// (no JetStream / missing account bucket); a forged token resolves to an
// Authenticated=false result, not an error.
func ResolveTokenReview(ctx context.Context, nc *nats.Conn, accountID, clusterName, token string, verifyTimeout time.Duration) (WebhookTokenReviewResult, error) {
	if nc == nil {
		return WebhookTokenReviewResult{}, fmt.Errorf("eks: ResolveTokenReview nil nats conn")
	}
	js, err := nc.JetStream()
	if err != nil {
		return WebhookTokenReviewResult{}, fmt.Errorf("eks: jetstream: %w", err)
	}
	kv, err := js.KeyValue(AccountBucketName(accountID))
	if err != nil {
		return WebhookTokenReviewResult{}, fmt.Errorf("eks: open account KV %s: %w", accountID, err)
	}

	verify := func(presignedURL string) (*TokenVerifyResponse, error) {
		return utils.NatsRequest[TokenVerifyResponse](
			ctx, nc, TokenVerifySubject,
			TokenVerifyRequest{PresignedURL: presignedURL, ClusterName: clusterName},
			verifyTimeout, "")
	}
	lookup := func(principalARN string) (*AccessEntryRecord, error) {
		return GetAccessEntryRecord(kv, clusterName, principalARN)
	}
	return Authenticate(token, verify, lookup), nil
}
