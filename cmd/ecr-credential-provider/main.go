// ecr-credential-provider is a kubelet exec credential provider that lets a
// Spinifex EKS worker node pull images from the internal Spinifex ECR registry.
//
// kubelet invokes it per-pull, feeding a CredentialProviderRequest on STDIN and
// reading a CredentialProviderResponse on STDOUT. The provider fetches the
// node IAM-role credentials from IMDS, SigV4-signs GetAuthorizationToken against
// the AWS gateway's ECR endpoint, and returns the minted AWS:<jwt> as Basic auth
// for the requested registry host.
//
// Static node config (gateway URL, CA, region) is read from the cloud-init env
// file /etc/spinifex-eks/agent.env (KEY=value); real env vars override it.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/internal/ecrauth"
	"github.com/mulgadc/spinifex/internal/imdscreds"

	_ "github.com/mulgadc/spinifex/internal/fipsboot"
)

const (
	credProviderAPIVersion = "credentialprovider.kubelet.k8s.io/v1"
	defaultCacheDuration   = "10h"
	// expiryMargin trims the token's real TTL so kubelet re-invokes the provider
	// before the JWT actually expires mid-run.
	expiryMargin = 30 * time.Minute

	defaultEnvFile   = "/etc/spinifex-eks/agent.env"
	defaultGatewayCA = "/etc/spinifex-eks/gateway-ca.pem"
	defaultIMDSBase  = "http://169.254.169.254/latest"
)

// CredentialProviderRequest is the kubelet exec-provider request on STDIN.
type CredentialProviderRequest struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Image      string `json:"image"`
}

// AuthConfig is a single registry credential entry.
type AuthConfig struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// CredentialProviderResponse is the kubelet exec-provider response on STDOUT.
type CredentialProviderResponse struct {
	APIVersion    string                `json:"apiVersion"`
	Kind          string                `json:"kind"`
	CacheKeyType  string                `json:"cacheKeyType"`
	CacheDuration string                `json:"cacheDuration"`
	Auth          map[string]AuthConfig `json:"auth"`
}

// config holds the static node settings the provider needs.
type config struct {
	GatewayURL string
	GatewayCA  string
	Region     string
	IMDSBase   string
}

func main() {
	cfg := loadConfig(defaultEnvFile)

	httpClient, err := ecrauth.GatewayHTTPClient(cfg.GatewayCA)
	if err != nil {
		slog.Error("ecr-credential-provider: build gateway client", "err", err)
		emitEmpty(os.Stdout)
		os.Exit(1)
	}

	if err := run(os.Stdin, os.Stdout, cfg, httpClient); err != nil {
		slog.Error("ecr-credential-provider: run failed", "err", err)
		// Still emit a valid empty-auth response so kubelet has a parseable reply.
		emitEmpty(os.Stdout)
		os.Exit(1)
	}
}

// run is the testable core: it reads a CredentialProviderRequest from in,
// resolves credentials via IMDS + the gateway, and writes a
// CredentialProviderResponse to out. The httpClient must trust the gateway CA.
func run(in io.Reader, out io.Writer, cfg config, httpClient *http.Client) error {
	var req CredentialProviderRequest
	if err := json.NewDecoder(in).Decode(&req); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	if req.Image == "" {
		return fmt.Errorf("request image is empty")
	}
	imageHost := ecrauth.HostFromImage(req.Image)

	creds, err := imdscreds.Fetch(httpClient, cfg.IMDSBase)
	if err != nil {
		return fmt.Errorf("fetch IMDS credentials: %w", err)
	}

	tok, err := ecrauth.GetAuthorizationToken(cfg.Region, cfg.GatewayURL, httpClient,
		creds.AccessKeyID, creds.SecretAccessKey, creds.SessionToken)
	if err != nil {
		return fmt.Errorf("get authorization token: %w", err)
	}

	auth := AuthConfig{Username: tok.Username, Password: tok.Password}
	authMap := map[string]AuthConfig{imageHost: auth}
	if tok.ProxyHost != "" && tok.ProxyHost != imageHost {
		authMap[tok.ProxyHost] = auth
	}

	resp := CredentialProviderResponse{
		APIVersion:    credProviderAPIVersion,
		Kind:          "CredentialProviderResponse",
		CacheKeyType:  "Registry",
		CacheDuration: cacheDurationFrom(tok.ExpiresAt),
		Auth:          authMap,
	}
	return json.NewEncoder(out).Encode(&resp)
}

// cacheDurationFrom derives a Go-duration string from the token expiry minus a
// safety margin, falling back to the default when expiry is unknown or too near.
func cacheDurationFrom(expiresAt time.Time) string {
	if expiresAt.IsZero() {
		return defaultCacheDuration
	}
	d := time.Until(expiresAt) - expiryMargin
	if d <= 0 {
		return defaultCacheDuration
	}
	return d.Round(time.Second).String()
}

// loadConfig reads the cloud-init env file then lets real env vars override. The
// IMDS base is fixed (not in the env file) but overridable for tests.
func loadConfig(envFile string) config {
	env := parseEnvFile(envFile)
	get := func(key string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return env[key]
	}

	cfg := config{
		GatewayURL: get("EKS_GATEWAY_URL"),
		GatewayCA:  get("EKS_GATEWAY_CA"),
		Region:     get("EKS_REGION"),
		IMDSBase:   get("EKS_IMDS_BASE"),
	}
	if cfg.GatewayCA == "" {
		cfg.GatewayCA = defaultGatewayCA
	}
	if cfg.IMDSBase == "" {
		cfg.IMDSBase = defaultIMDSBase
	}
	return cfg
}

// parseEnvFile reads a simple KEY=value file; missing files yield an empty map.
func parseEnvFile(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(key)] = strings.TrimSpace(val)
	}
	return out
}

// emitEmpty writes a valid CredentialProviderResponse with no auth entries.
func emitEmpty(out io.Writer) {
	resp := CredentialProviderResponse{
		APIVersion:    credProviderAPIVersion,
		Kind:          "CredentialProviderResponse",
		CacheKeyType:  "Registry",
		CacheDuration: defaultCacheDuration,
		Auth:          map[string]AuthConfig{},
	}
	if err := json.NewEncoder(out).Encode(&resp); err != nil {
		slog.Error("ecr-credential-provider: encode empty response", "err", err)
	}
}
