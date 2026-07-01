// eks-token-webhook is the K8s TokenReview webhook authenticator baked into
// the eks-server AMI. Per kubectl call the kube-apiserver POSTs the bearer
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
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/mulgadc/spinifex/internal/eksgw"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
)

// config is the webhook's runtime configuration, sourced from the first-boot
// env (/etc/spinifex-eks/first-boot.env) cloud-init seeds onto the VM.
type config struct {
	addr        string
	gatewayURL  string
	gatewayCA   string
	accessKey   string
	secretKey   string
	region      string
	accountID   string
	clusterName string
	certPath    string
	keyPath     string
	kubeconfig  string
}

func loadConfig() (config, error) {
	c := config{
		addr:        flagAddr,
		gatewayURL:  os.Getenv("EKS_GATEWAY_URL"),
		gatewayCA:   os.Getenv("EKS_GATEWAY_CA"),
		accessKey:   os.Getenv("EKS_ACCESS_KEY"),
		secretKey:   os.Getenv("EKS_SECRET_KEY"),
		region:      os.Getenv("EKS_REGION"),
		accountID:   os.Getenv("EKS_ACCOUNT_ID"),
		clusterName: os.Getenv("EKS_CLUSTER_NAME"),
		certPath:    envOr("EKS_WEBHOOK_CERT", "/etc/spinifex-eks/token-webhook.crt"),
		keyPath:     envOr("EKS_WEBHOOK_KEY", "/etc/spinifex-eks/token-webhook.key"),
		kubeconfig:  envOr("EKS_WEBHOOK_KUBECONFIG", "/etc/spinifex-eks/token-webhook.kubeconfig"),
	}
	// Static SigV4 creds are optional: when absent, eksgw.New signs with the
	// AWS SDK chain (IMDS instance-role creds), the same path the CP VM's
	// sibling helpers use. The CP VM launches on an instance profile, so
	// buildK3sUserData omits EKS_ACCESS_KEY/EKS_SECRET_KEY by design.
	switch {
	case c.gatewayURL == "":
		return c, errors.New("EKS_GATEWAY_URL not set")
	case c.accountID == "":
		return c, errors.New("EKS_ACCOUNT_ID not set")
	case c.clusterName == "":
		return c, errors.New("EKS_CLUSTER_NAME not set")
	}
	return c, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

var flagAddr string

func main() {
	flag.StringVar(&flagAddr, "addr", "127.0.0.1:8443", "listen address (loopback only)")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg, err := loadConfig()
	if err != nil {
		slog.Error("eks-token-webhook config error", "err", err)
		os.Exit(1)
	}

	if err := run(cfg); err != nil {
		slog.Error("eks-token-webhook fatal", "err", err)
		os.Exit(1)
	}
}

func run(cfg config) error {
	// Serving cert + apiserver kubeconfig. Persisted and reused across restarts
	// so a webhook crash does not invalidate the CA the apiserver already loaded.
	tlsCert, certPEM, err := ensureServingCert(cfg.certPath, cfg.keyPath)
	if err != nil {
		return fmt.Errorf("serving cert: %w", err)
	}
	if err := writeAPIServerKubeconfig(cfg.kubeconfig, cfg.addr, certPEM); err != nil {
		return fmt.Errorf("write apiserver kubeconfig: %w", err)
	}

	client, err := eksgw.New(cfg.gatewayURL, cfg.gatewayCA, cfg.accessKey, cfg.secretKey, cfg.region)
	if err != nil {
		return fmt.Errorf("build gateway client: %w", err)
	}

	authr := &authenticator{
		accountID:   cfg.accountID,
		clusterName: cfg.clusterName,
		review:      gatewayReviewer(client, cfg.clusterName, cfg.accountID),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/authenticate", authr.handle)

	server := &http.Server{
		Addr:              cfg.addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig:         tlsCert.serverTLSConfig(),
	}
	slog.Info("eks-token-webhook listening", "addr", cfg.addr, "cluster", cfg.clusterName)
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
