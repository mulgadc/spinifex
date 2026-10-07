// Package tokenwebhook implements eks-token-webhook, the K8s TokenReview webhook
// authenticator baked into the eks-server AMI. Per kubectl call the kube-apiserver POSTs the bearer
// token (produced by `aws eks get-token`); the webhook relays it through the
// AWS gateway broker (SigV4 HTTPS POST to /clusters/{name}/token-review), which
// host-side runs the STS verify (cross-cluster x-k8s-aws-id pin) + AccessEntry
// KV lookup and returns the resolved identity. The webhook never speaks core
// NATS — same gateway-broker model as ELBv2's lb-agent and eks-gateway-publish.
//
// It binds 127.0.0.1 only; the kube-apiserver is the sole client (loopback).
// On startup it writes the apiserver webhook kubeconfig (with its self-signed
// serving CA) so k3s can be pointed at it via
// --authentication-token-webhook-config-file.
//
// SigV4 creds come from the AWS SDK chain (IMDS instance-role) when static
// EKS_ACCESS_KEY/EKS_SECRET_KEY are absent, matching the sibling helpers.
package tokenwebhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/mulgadc/spinifex/internal/eksgw"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
)

// Config is the webhook's runtime configuration; the binary sources it from
// the first-boot env (/etc/spinifex-eks/first-boot.env) cloud-init seeds onto
// the VM.
type Config struct {
	Addr        string
	GatewayURL  string
	GatewayCA   string
	AccessKey   string
	SecretKey   string
	Region      string
	AccountID   string
	ClusterName string
	CertPath    string
	KeyPath     string
	Kubeconfig  string
}

// Run writes the serving certificate and apiserver kubeconfig, then serves
// TokenReview on cfg.Addr until the server fails or ctx is cancelled.
func Run(ctx context.Context, cfg Config) error {
	// Serving cert + apiserver kubeconfig. Persisted and reused across restarts
	// so a webhook crash does not invalidate the CA the apiserver already loaded.
	tlsCert, certPEM, err := ensureServingCert(cfg.CertPath, cfg.KeyPath)
	if err != nil {
		return fmt.Errorf("serving cert: %w", err)
	}
	if err := writeAPIServerKubeconfig(cfg.Kubeconfig, cfg.Addr, certPEM); err != nil {
		return fmt.Errorf("write apiserver kubeconfig: %w", err)
	}

	client, err := eksgw.New(cfg.GatewayURL, cfg.GatewayCA, cfg.AccessKey, cfg.SecretKey, cfg.Region)
	if err != nil {
		return fmt.Errorf("build gateway client: %w", err)
	}

	authr := &authenticator{
		accountID:   cfg.AccountID,
		clusterName: cfg.ClusterName,
		review:      gatewayReviewer(client, cfg.ClusterName, cfg.AccountID),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/authenticate", authr.handle)

	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig:         serverTLSConfig(tlsCert),
	}
	stop := context.AfterFunc(ctx, func() { _ = server.Close() })
	defer stop()
	slog.Info("eks-token-webhook listening", "addr", cfg.Addr, "cluster", cfg.ClusterName)
	if err := server.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// tokenReviewRequest is the body POSTed to /clusters/{name}/token-review. The
// webhook holds system SigV4 creds (system account, not the cluster account),
// so accountId names the cluster account explicitly — same as PublishInternal.
type tokenReviewRequest struct {
	AccountID string `json:"accountId"`
	Token     string `json:"token"`
}

// gatewayReviewer returns the per-call relay: SigV4-POST {accountId,token} to
// the gateway and decode the resolved identity. A non-2xx / transport error is
// surfaced (handle() turns it into a 5xx so the apiserver retries) rather than a
// silent deny, distinguishing "webhook broker down" from "token rejected".
func gatewayReviewer(client *eksgw.Client, clusterName, accountID string) func(string) (handlers_eks.WebhookTokenReviewResult, error) {
	path := "/clusters/" + clusterName + "/token-review"
	return func(token string) (handlers_eks.WebhookTokenReviewResult, error) {
		body, err := json.Marshal(tokenReviewRequest{AccountID: accountID, Token: token})
		if err != nil {
			return handlers_eks.WebhookTokenReviewResult{}, fmt.Errorf("marshal request: %w", err)
		}
		respBody, err := client.Post(path, body)
		if err != nil {
			return handlers_eks.WebhookTokenReviewResult{}, err
		}
		var res handlers_eks.WebhookTokenReviewResult
		if err := json.Unmarshal(respBody, &res); err != nil {
			return handlers_eks.WebhookTokenReviewResult{}, fmt.Errorf("decode response: %w", err)
		}
		return res, nil
	}
}

// authenticator resolves a TokenReview by relaying to the gateway broker.
type authenticator struct {
	accountID   string
	clusterName string
	review      func(token string) (handlers_eks.WebhookTokenReviewResult, error)
}

func (a *authenticator) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var review tokenReview
	if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	res, err := a.review(review.Spec.Token)
	if err != nil {
		// Broker fault (gateway down, transport error): a 5xx tells the apiserver
		// to retry rather than treating a transient outage as a hard deny.
		slog.Error("TokenReview relay failed", "cluster", a.clusterName, "err", err)
		http.Error(w, "token review unavailable", http.StatusServiceUnavailable)
		return
	}

	status := tokenReviewStatus{Authenticated: res.Authenticated}
	if res.Authenticated {
		status.User = userInfo{
			Username: res.Username,
			UID:      res.UID,
			Groups:   res.Groups,
		}
		// Confirm the token for the audiences the apiserver asked about; an empty
		// status.audiences is rejected when --api-audiences is configured.
		status.Audiences = review.Spec.Audiences
	}
	slog.Info("TokenReview decision", "authenticated", status.Authenticated, "username", status.User.Username, "audiences", status.Audiences)

	resp := tokenReview{
		APIVersion: "authentication.k8s.io/v1",
		Kind:       "TokenReview",
		Status:     status,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Error("encode TokenReview response", "err", err)
	}
}
