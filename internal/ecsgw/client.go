// Package ecsgw is the on-VM SigV4 client the ecs-agent uses to reach the AWS
// gateway over HTTPS instead of connecting to NATS directly. It signs requests
// for service "ecs" with the instance's IMDS instance-role credentials (fetched
// fresh per call so rotation is transparent), keeping the NATS bus host-internal
// (the gateway relays agent calls onto ecs.bus.* host-side).
package ecsgw

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws/credentials"
	v4 "github.com/aws/aws-sdk-go/aws/signer/v4"

	"github.com/mulgadc/spinifex/internal/tlsconfig"
)

// targetPrefix is the X-Amz-Target service prefix the gateway's ECS dispatch
// strips to the bare action. Any prefix works (ecsActionFromTarget takes the
// suffix); this one mirrors the AWS JSON 1.1 convention.
const targetPrefix = "AmazonEC2ContainerServiceV20141113."

// CredentialsFunc yields the current SigV4 credentials. It is called once per
// request so a provider backed by IMDS can rotate instance-role creds (and their
// session token) transparently. Must be safe for concurrent use.
type CredentialsFunc func(ctx context.Context) (accessKey, secretKey, sessionToken string, err error)

// Client posts SigV4-signed AWS JSON 1.1 requests to the gateway for service
// "ecs". One client is reused for register/heartbeat/state/poll.
type Client struct {
	baseURL    string
	creds      CredentialsFunc
	region     string
	httpClient *http.Client
}

// New builds a client. caPath optionally pins the gateway TLS CA; empty relies on
// the system trust store. creds supplies the per-call instance-role credentials.
// region defaults to us-east-1 when empty (SigV4 requires a non-empty region).
// timeout bounds a single call — callers needing a long-poll pass a larger value
// than the default register/heartbeat timeout.
func New(baseURL, caPath string, creds CredentialsFunc, region string, timeout time.Duration) (*Client, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("ecsgw: baseURL is required")
	}
	if creds == nil {
		return nil, fmt.Errorf("ecsgw: credentials func is required")
	}
	if region == "" {
		region = "us-east-1"
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	tlsCfg := &tls.Config{
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: tlsconfig.Curves,
	}
	if caPath != "" {
		pem, err := os.ReadFile(caPath)
		if err != nil {
			return nil, fmt.Errorf("ecsgw: read gateway CA %q: %w", caPath, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ecsgw: gateway CA %q has no usable certificates", caPath)
		}
		tlsCfg.RootCAs = pool
	}

	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		creds:   creds,
		region:  region,
		httpClient: &http.Client{
			Timeout:   timeout,
			Transport: &http.Transport{TLSClientConfig: tlsCfg, MaxIdleConns: 2, IdleConnTimeout: 30 * time.Second},
		},
	}, nil
}

// Call SigV4-signs and POSTs body under X-Amz-Target "<prefix><action>" to the
// gateway root, returning the 2xx response body. Credentials are fetched per call
// and the v4 signer emits X-Amz-Security-Token for instance-role (temporary)
// creds, so the gateway's ASIA path verifies them. No retry; callers wrap.
func (c *Client) Call(action string, body []byte) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", targetPrefix+action)

	akid, secret, token, err := c.creds(req.Context())
	if err != nil {
		return nil, fmt.Errorf("retrieve credentials: %w", err)
	}
	signer := v4.NewSigner(credentials.NewStaticCredentials(akid, secret, token))
	if _, err := signer.Sign(req, bytes.NewReader(body), "ecs", c.region, time.Now()); err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gateway returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return respBody, nil
}
