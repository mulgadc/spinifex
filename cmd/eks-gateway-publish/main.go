// eks-gateway-publish runs inside the EKS K3s control-plane VM and relays a
// single publication to the host through the AWS gateway, instead of dialing
// core NATS directly.
//
// It reads a JSON payload on stdin, wraps it as
// {accountId, channel, kind, payload}, SigV4-signs (service "eks") an HTTPS
// POST to {gateway}/clusters/{cluster}/internal-publish, and retries with
// backoff until the gateway returns 2xx or the attempt budget is exhausted —
// so a degraded link surfaces as a non-zero exit rather than a silently
// dropped message (the failure mode of fire-and-forget `nats pub`).
//
// Usage:
//
//	echo '{"token":"..."}' | eks-gateway-publish -channel bootstrap -kind k3s-bootstrap-token
//	kubectl ... | eks-gateway-publish -channel state
//
// Flags default to environment variables seeded by cloud-init:
// EKS_GATEWAY_URL, EKS_GATEWAY_CA, EKS_ACCESS_KEY, EKS_SECRET_KEY, EKS_REGION,
// EKS_ACCOUNT_ID, EKS_CLUSTER_NAME.
package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mulgadc/predastore/auth"
	"github.com/mulgadc/spinifex/internal/tlsconfig"

	_ "github.com/mulgadc/spinifex/internal/fipsboot"
)

const (
	// Retry budget: the control plane reaches readiness before this runs, so a
	// failing POST means a degraded link, not a cold start. Bounded so a stuck
	// boot still terminates the OpenRC service.
	maxAttempts = 30
	retryDelay  = 5 * time.Second
	httpTimeout = 10 * time.Second
)

type publishBody struct {
	AccountID string          `json:"accountId"`
	Channel   string          `json:"channel"`
	Kind      string          `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
}

func main() {
	var (
		gatewayURL  string
		gatewayCA   string
		accessKey   string
		secretKey   string
		region      string
		accountID   string
		clusterName string
		channel     string
		kind        string
	)

	flag.StringVar(&gatewayURL, "gateway", os.Getenv("EKS_GATEWAY_URL"), "Gateway URL (e.g. https://10.15.8.1:9999)")
	flag.StringVar(&gatewayCA, "gateway-ca", os.Getenv("EKS_GATEWAY_CA"), "Path to gateway TLS CA PEM (optional; falls back to system trust)")
	flag.StringVar(&accessKey, "access-key", os.Getenv("EKS_ACCESS_KEY"), "AWS access key ID")
	flag.StringVar(&secretKey, "secret-key", os.Getenv("EKS_SECRET_KEY"), "AWS secret access key")
	flag.StringVar(&region, "region", envOrDefault("EKS_REGION", "us-east-1"), "AWS region for SigV4 signing")
	flag.StringVar(&accountID, "account-id", os.Getenv("EKS_ACCOUNT_ID"), "Cluster account ID")
	flag.StringVar(&clusterName, "cluster", os.Getenv("EKS_CLUSTER_NAME"), "Cluster name")
	flag.StringVar(&channel, "channel", "", "Publish channel: bootstrap|state")
	flag.StringVar(&kind, "kind", "", "Bootstrap subject kind (bootstrap channel only)")
	flag.Parse()

	switch {
	case gatewayURL == "":
		fatal("--gateway is required (or set EKS_GATEWAY_URL)")
	case accessKey == "" || secretKey == "":
		fatal("--access-key and --secret-key are required (or set EKS_ACCESS_KEY / EKS_SECRET_KEY)")
	case accountID == "":
		fatal("--account-id is required (or set EKS_ACCOUNT_ID)")
	case clusterName == "":
		fatal("--cluster is required (or set EKS_CLUSTER_NAME)")
	case channel != "bootstrap" && channel != "state":
		fatal("--channel must be bootstrap or state")
	case channel == "bootstrap" && kind == "":
		fatal("--kind is required for the bootstrap channel")
	}

	payload, err := io.ReadAll(os.Stdin)
	if err != nil {
		fatal(fmt.Sprintf("read stdin payload: %v", err))
	}
	if len(bytes.TrimSpace(payload)) == 0 {
		fatal("empty stdin payload")
	}
	if !json.Valid(payload) {
		fatal("stdin payload is not valid JSON")
	}

	body, err := json.Marshal(publishBody{
		AccountID: accountID,
		Channel:   channel,
		Kind:      kind,
		Payload:   json.RawMessage(payload),
	})
	if err != nil {
		fatal(fmt.Sprintf("marshal request body: %v", err))
	}

	client, err := newClient(gatewayCA)
	if err != nil {
		fatal(fmt.Sprintf("build HTTP client: %v", err))
	}

	url := strings.TrimRight(gatewayURL, "/") + "/clusters/" + clusterName + "/internal-publish"

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := post(client, url, body, accessKey, secretKey, region); err != nil {
			lastErr = err
			slog.Warn("eks-gateway-publish: attempt failed",
				"channel", channel, "kind", kind, "attempt", attempt, "err", err)
			if attempt < maxAttempts {
				time.Sleep(retryDelay)
			}
			continue
		}
		slog.Info("eks-gateway-publish: published", "channel", channel, "kind", kind)
		return
	}
	fatal(fmt.Sprintf("publish failed after %d attempts: %v", maxAttempts, lastErr))
}

func post(client *http.Client, url string, body []byte, accessKey, secretKey, region string) error {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")

	sum := sha256.Sum256(body)
	if err := auth.SignReq(req, accessKey, secretKey, hex.EncodeToString(sum[:]), "eks", region); err != nil {
		return fmt.Errorf("sign request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("gateway returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// newClient builds an HTTPS client. When caPath is set it pins the gateway CA;
// otherwise it relies on the system trust store (CA injected via cloud-init).
func newClient(caPath string) (*http.Client, error) {
	tlsCfg := &tls.Config{
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: tlsconfig.Curves,
	}
	if caPath != "" {
		pem, err := os.ReadFile(caPath)
		if err != nil {
			return nil, fmt.Errorf("read gateway CA %q: %w", caPath, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("gateway CA %q has no usable certificates", caPath)
		}
		tlsCfg.RootCAs = pool
	}
	return &http.Client{
		Timeout:   httpTimeout,
		Transport: &http.Transport{TLSClientConfig: tlsCfg, MaxIdleConns: 1, IdleConnTimeout: 30 * time.Second},
	}, nil
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func fatal(msg string) {
	slog.Error("eks-gateway-publish: " + msg)
	os.Exit(1)
}
