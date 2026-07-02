// Package eksgw is the on-VM SigV4 client used by EKS helpers (eks-gateway-publish,
// eks-token-webhook) to POST over HTTPS to the AWS gateway instead of NATS.
// Signs for service "eks" with either static keys or IMDS instance-role creds.
package eksgw

import (
	"bytes"
	"context"
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

	"github.com/mulgadc/spinifex/internal/gwsign"
	"github.com/mulgadc/spinifex/internal/tlsconfig"
)

// Client posts SigV4-signed requests to the AWS gateway for the "eks" service.
type Client struct {
	baseURL    string
	signer     *gwsign.Signer
	region     string
	httpClient *http.Client
}

// New builds a client. caPath optionally pins the gateway TLS CA; when empty the
// client relies on the system trust store. region defaults to us-east-1 when
// empty (SigV4 requires a non-empty region). When accessKey/secretKey are empty
// the client signs with IMDS instance-role credentials served in-VM.
func New(baseURL, caPath, accessKey, secretKey, region string) (*Client, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("eksgw: baseURL is required")
	}
	if region == "" {
		region = "us-east-1"
	}

	var signer *gwsign.Signer
	if accessKey != "" && secretKey != "" {
		signer = gwsign.NewStatic(accessKey, secretKey)
	} else {
		s, err := gwsign.NewIMDS(context.Background(), region)
		if err != nil {
			return nil, fmt.Errorf("eksgw: init IMDS signer: %w", err)
		}
		signer = s
	}

	tlsCfg := &tls.Config{
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: tlsconfig.Curves,
	}
	if caPath != "" {
		pem, err := os.ReadFile(caPath)
		if err != nil {
			return nil, fmt.Errorf("eksgw: read gateway CA %q: %w", caPath, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("eksgw: gateway CA %q has no usable certificates", caPath)
		}
		tlsCfg.RootCAs = pool
	}

	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		signer:  signer,
		region:  region,
		httpClient: &http.Client{
			Timeout:   10 * time.Second,
			Transport: &http.Transport{TLSClientConfig: tlsCfg, MaxIdleConns: 2, IdleConnTimeout: 30 * time.Second},
		},
	}, nil
}

// Post SigV4-signs and sends body to path, returning the response body on 2xx.
// Errors include the gateway status and body. No retry; callers wrap as needed.
func (c *Client) Post(path string, body []byte) ([]byte, error) {
	return c.do(http.MethodPost, path, body)
}

// Get SigV4-signs and sends a single GET to path (e.g.
// "/clusters/alpha/internal-addons?accountId=..."). Same contract as Post: 2xx
// body or an error carrying the gateway status. No retry.
func (c *Client) Get(path string) ([]byte, error) {
	return c.do(http.MethodGet, path, nil)
}

func (c *Client) do(method, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequest(method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	}

	sum := sha256.Sum256(body)
	if err := c.signer.Sign(req, hex.EncodeToString(sum[:]), "eks", c.region); err != nil {
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
