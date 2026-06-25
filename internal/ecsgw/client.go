// Package ecsgw is the on-VM SigV4 client the ecs-agent uses to reach the AWS
// gateway over HTTPS instead of connecting to NATS directly. It signs requests
// for service "ecs" with the instance's seeded IAM credentials, keeping the NATS
// bus host-internal (the gateway relays agent calls onto ecs.bus.* host-side).
package ecsgw

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mulgadc/predastore/auth"
	"github.com/mulgadc/spinifex/internal/tlsconfig"
)

// targetPrefix is the X-Amz-Target service prefix the gateway's ECS dispatch
// strips to the bare action. Any prefix works (ecsActionFromTarget takes the
// suffix); this one mirrors the AWS JSON 1.1 convention.
const targetPrefix = "AmazonEC2ContainerServiceV20141113."

// Client posts SigV4-signed AWS JSON 1.1 requests to the gateway for service
// "ecs". One client is reused for register/heartbeat/state/poll.
type Client struct {
	baseURL    string
	accessKey  string
	secretKey  string
	region     string
	httpClient *http.Client
}

// New builds a client. caPath optionally pins the gateway TLS CA; empty relies on
// the system trust store. region defaults to us-east-1 when empty (SigV4 requires
// a non-empty region). timeout bounds a single call — callers needing a long-poll
// pass a larger value than the default register/heartbeat timeout.
func New(baseURL, caPath, accessKey, secretKey, region string, timeout time.Duration) (*Client, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("ecsgw: baseURL is required")
	}
	if accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("ecsgw: access key and secret key are required")
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
		baseURL:   strings.TrimRight(baseURL, "/"),
		accessKey: accessKey,
		secretKey: secretKey,
		region:    region,
		httpClient: &http.Client{
			Timeout:   timeout,
			Transport: &http.Transport{TLSClientConfig: tlsCfg, MaxIdleConns: 2, IdleConnTimeout: 30 * time.Second},
		},
	}, nil
}

// Call SigV4-signs and POSTs body under X-Amz-Target "<prefix><action>" to the
// gateway root, returning the 2xx response body. Errors carry the gateway status
// and body. No retry; callers wrap as needed.
func (c *Client) Call(action string, body []byte) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", targetPrefix+action)

	sum := sha256.Sum256(body)
	if err := auth.SignReq(req, c.accessKey, c.secretKey, hex.EncodeToString(sum[:]), "ecs", c.region); err != nil {
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
