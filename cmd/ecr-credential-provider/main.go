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
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/mulgadc/spinifex/internal/tlsconfig"

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
	imdsTokenTTL     = "21600"
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

	httpClient, err := gatewayHTTPClient(cfg.GatewayCA)
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
	imageHost := hostFromImage(req.Image)

	creds, err := fetchIMDSCredentials(httpClient, cfg.IMDSBase)
	if err != nil {
		return fmt.Errorf("fetch IMDS credentials: %w", err)
	}

	username, password, proxyHost, expiresAt, err := getAuthToken(cfg, httpClient, creds)
	if err != nil {
		return fmt.Errorf("get authorization token: %w", err)
	}

	auth := AuthConfig{Username: username, Password: password}
	authMap := map[string]AuthConfig{imageHost: auth}
	if proxyHost != "" && proxyHost != imageHost {
		authMap[proxyHost] = auth
	}

	resp := CredentialProviderResponse{
		APIVersion:    credProviderAPIVersion,
		Kind:          "CredentialProviderResponse",
		CacheKeyType:  "Registry",
		CacheDuration: cacheDurationFrom(expiresAt),
		Auth:          authMap,
	}
	return json.NewEncoder(out).Encode(&resp)
}

// imdsCreds mirrors the IMDS security-credentials JSON shape.
type imdsCreds struct {
	Code            string `json:"Code"`
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	Token           string `json:"Token"`
	Expiration      string `json:"Expiration"`
}

// fetchIMDSCredentials resolves the node role name then its credentials via
// IMDSv2 (PUT token, then GET with the token header).
func fetchIMDSCredentials(client *http.Client, base string) (imdsCreds, error) {
	base = strings.TrimRight(base, "/")
	token := imdsV2Token(client, base)

	roleName, err := imdsGet(client, base+"/meta-data/iam/security-credentials/", token)
	if err != nil {
		return imdsCreds{}, fmt.Errorf("fetch role name: %w", err)
	}
	roleName = strings.TrimSpace(roleName)
	if roleName == "" {
		return imdsCreds{}, fmt.Errorf("IMDS returned empty role name")
	}

	body, err := imdsGet(client, base+"/meta-data/iam/security-credentials/"+roleName, token)
	if err != nil {
		return imdsCreds{}, fmt.Errorf("fetch role credentials: %w", err)
	}
	var creds imdsCreds
	if err := json.Unmarshal([]byte(body), &creds); err != nil {
		return imdsCreds{}, fmt.Errorf("decode role credentials: %w", err)
	}
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		return imdsCreds{}, fmt.Errorf("IMDS credentials missing access/secret key")
	}
	return creds, nil
}

// imdsV2Token requests an IMDSv2 session token; returns "" if the service does
// not enforce v2 (tokenless v1 then applies).
func imdsV2Token(client *http.Client, base string) string {
	req, err := http.NewRequest(http.MethodPut, base+"/api/token", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("X-Aws-Ec2-Metadata-Token-Ttl-Seconds", imdsTokenTTL)
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(body))
}

func imdsGet(client *http.Client, url, token string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		req.Header.Set("X-Aws-Ec2-Metadata-Token", token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("IMDS %s returned %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return string(body), nil
}

// getAuthToken builds an aws-sdk ECR client pointed at the gateway and calls
// GetAuthorizationToken, decoding the AWS:<jwt> token into Basic-auth parts. It
// returns the username, password, the ProxyEndpoint host, and the token expiry.
func getAuthToken(cfg config, httpClient *http.Client, creds imdsCreds) (username, password, proxyHost string, expiresAt time.Time, err error) {
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	sess, serr := session.NewSession(&aws.Config{
		Region:      aws.String(region),
		Endpoint:    aws.String(cfg.GatewayURL),
		Credentials: credentials.NewStaticCredentials(creds.AccessKeyID, creds.SecretAccessKey, creds.Token),
		HTTPClient:  httpClient,
		DisableSSL:  aws.Bool(false),
	})
	if serr != nil {
		return "", "", "", time.Time{}, fmt.Errorf("new session: %w", serr)
	}

	out, gerr := ecr.New(sess).GetAuthorizationToken(&ecr.GetAuthorizationTokenInput{})
	if gerr != nil {
		return "", "", "", time.Time{}, fmt.Errorf("GetAuthorizationToken: %w", gerr)
	}
	if len(out.AuthorizationData) == 0 || out.AuthorizationData[0].AuthorizationToken == nil {
		return "", "", "", time.Time{}, fmt.Errorf("GetAuthorizationToken returned no authorization data")
	}
	data := out.AuthorizationData[0]

	raw, derr := base64.StdEncoding.DecodeString(aws.StringValue(data.AuthorizationToken))
	if derr != nil {
		return "", "", "", time.Time{}, fmt.Errorf("decode authorization token: %w", derr)
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok {
		return "", "", "", time.Time{}, fmt.Errorf("authorization token not in user:password form")
	}

	if data.ExpiresAt != nil {
		expiresAt = aws.TimeValue(data.ExpiresAt)
	}
	return user, pass, hostFromImage(aws.StringValue(data.ProxyEndpoint)), expiresAt, nil
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

// gatewayHTTPClient builds an HTTP client trusting the gateway CA at caPath.
// When caPath is empty it relies on the system trust store.
func gatewayHTTPClient(caPath string) (*http.Client, error) {
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
		Timeout:   15 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg, MaxIdleConns: 2, IdleConnTimeout: 30 * time.Second},
	}, nil
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

// hostFromImage returns the host[:port] portion of an image ref or URL. It strips
// any scheme and trims at the first path separator.
func hostFromImage(image string) string {
	if i := strings.Index(image, "://"); i >= 0 {
		image = image[i+3:]
	}
	if i := strings.IndexByte(image, '/'); i >= 0 {
		image = image[:i]
	}
	return image
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
