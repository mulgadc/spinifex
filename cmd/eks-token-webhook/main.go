// eks-token-webhook is the K8s TokenReview webhook authenticator baked into
// the eks-server AMI. It decodes the SigV4-presigned GetCallerIdentity URL
// produced by `aws eks get-token`, calls Mulga STS over NATS (which enforces
// the x-k8s-aws-id cross-cluster pin against this cluster's name), and resolves
// the principal ARN to a TokenReview response via the cluster's AccessEntry KV.
//
// It binds 127.0.0.1 only; the kube-apiserver is the sole client (loopback).
// On startup it writes the apiserver webhook kubeconfig (with its self-signed
// serving CA) so k3s can be pointed at it via
// --authentication-token-webhook-config-file.
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

	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// config is the webhook's runtime configuration, sourced from the first-boot
// env (/etc/spinifex-eks/first-boot.env) cloud-init seeds onto the VM.
type config struct {
	addr        string
	natsURL     string
	natsToken   string
	natsCA      string
	accountID   string
	clusterName string
	certPath    string
	keyPath     string
	kubeconfig  string
	verifyTO    time.Duration
}

func loadConfig() (config, error) {
	c := config{
		addr:        flagAddr,
		natsURL:     os.Getenv("SPINIFEX_NATS_URL"),
		natsToken:   os.Getenv("SPINIFEX_NATS_TOKEN"),
		natsCA:      os.Getenv("SPINIFEX_NATS_CA"),
		accountID:   os.Getenv("EKS_ACCOUNT_ID"),
		clusterName: os.Getenv("EKS_CLUSTER_NAME"),
		certPath:    envOr("EKS_WEBHOOK_CERT", "/etc/spinifex-eks/token-webhook.crt"),
		keyPath:     envOr("EKS_WEBHOOK_KEY", "/etc/spinifex-eks/token-webhook.key"),
		kubeconfig:  envOr("EKS_WEBHOOK_KUBECONFIG", "/etc/spinifex-eks/token-webhook.kubeconfig"),
		verifyTO:    5 * time.Second,
	}
	if c.natsURL == "" {
		return c, errors.New("SPINIFEX_NATS_URL not set")
	}
	if c.accountID == "" {
		return c, errors.New("EKS_ACCOUNT_ID not set")
	}
	if c.clusterName == "" {
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

	nc, err := utils.ConnectNATSWithRetry(cfg.natsURL, cfg.natsToken, cfg.natsCA)
	if err != nil {
		return fmt.Errorf("connect NATS: %w", err)
	}
	defer nc.Close()

	kv, err := openAccountKV(nc, cfg.accountID)
	if err != nil {
		return fmt.Errorf("open account KV: %w", err)
	}

	authr := &authenticator{
		nc:          nc,
		kv:          kv,
		clusterName: cfg.clusterName,
		verifyTO:    cfg.verifyTO,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/authenticate", authr.handle)

	server := newServer(cfg.addr, mux, tlsCert)
	slog.Info("eks-token-webhook listening", "addr", cfg.addr, "cluster", cfg.clusterName)
	if err := server.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// openAccountKV attaches to the per-account EKS KV bucket. The bucket already
// exists by the time a control-plane VM is running (CreateCluster created it),
// but JetStream may briefly lag at boot, so retry a few times.
func openAccountKV(nc *nats.Conn, accountID string) (nats.KeyValue, error) {
	js, err := nc.JetStream()
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	bucket := handlers_eks.AccountBucketName(accountID)
	var lastErr error
	for range 30 {
		kv, kvErr := js.KeyValue(bucket)
		if kvErr == nil {
			return kv, nil
		}
		lastErr = kvErr
		time.Sleep(2 * time.Second)
	}
	return nil, fmt.Errorf("kv bucket %s not available: %w", bucket, lastErr)
}

// authenticator resolves a TokenReview against STS (over NATS) + AccessEntry KV.
type authenticator struct {
	nc          *nats.Conn
	kv          nats.KeyValue
	clusterName string
	verifyTO    time.Duration
}

// verify resolves a presigned get-token URL into the caller principal via the
// awsgw-hosted STS verify subject, binding the signature to this cluster name.
func (a *authenticator) verify(presignedURL string) (*handlers_eks.TokenVerifyResponse, error) {
	req := handlers_eks.TokenVerifyRequest{
		PresignedURL: presignedURL,
		ClusterName:  a.clusterName,
	}
	// accountID header is unused by the verify responder; pass empty.
	return utils.NATSRequest[handlers_eks.TokenVerifyResponse](
		a.nc, handlers_eks.TokenVerifySubject, req, a.verifyTO, "")
}

// lookup reads the AccessEntry for a principal ARN from the cluster KV.
func (a *authenticator) lookup(principalARN string) (*handlers_eks.AccessEntryRecord, error) {
	return handlers_eks.GetAccessEntryRecord(a.kv, a.clusterName, principalARN)
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

	status := authenticate(review.Spec.Token, a.verify, a.lookup)
	slog.Info("TokenReview decision", "authenticated", status.Authenticated, "username", status.User.Username)

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

// authenticate is the pure decision core: decode token → STS verify → KV
// AccessEntry lookup → TokenReview status. Any failure yields an unauthenticated
// status (never a 5xx) so a forged/unknown token is a clean K8s 401, not a
// webhook outage. verify and lookup are injected so the logic is unit-testable.
func authenticate(
	token string,
	verify func(presignedURL string) (*handlers_eks.TokenVerifyResponse, error),
	lookup func(principalARN string) (*handlers_eks.AccessEntryRecord, error),
) tokenReviewStatus {
	presignedURL, err := handlers_eks.DecodeGetToken(token)
	if err != nil {
		return tokenReviewStatus{Authenticated: false}
	}
	ident, err := verify(presignedURL)
	if err != nil {
		slog.Debug("token verify rejected", "err", err)
		return tokenReviewStatus{Authenticated: false}
	}
	rec, err := lookup(ident.ARN)
	if err != nil {
		// No AccessEntry for an otherwise-valid IAM principal: authenticated to
		// AWS, but not granted any K8s identity on this cluster.
		slog.Debug("no access entry for principal", "arn", ident.ARN, "err", err)
		return tokenReviewStatus{Authenticated: false}
	}
	uid := ident.UserID
	if uid == "" {
		uid = ident.ARN
	}
	return tokenReviewStatus{
		Authenticated: true,
		User: userInfo{
			Username: rec.KubernetesUsername,
			UID:      uid,
			Groups:   rec.KubernetesGroups,
		},
	}
}

// newServer builds the loopback TLS server. Split out so tests can construct it
// without binding a port.
func newServer(addr string, handler http.Handler, cert tlsCertificate) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig:         cert.serverTLSConfig(),
	}
}
