// Package rdsgw is the on-VM SigV4 client the rds-agent uses to reach the AWS
// gateway over HTTPS, keeping NATS host-internal. Unlike internal/ecsgw it
// speaks the AWS Query protocol: a form-encoded Action= POST answered with the
// IAM-style <ActionResponse><ActionResult> envelope.
package rdsgw

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/internal/gwsign"
	"github.com/mulgadc/spinifex/internal/tlsconfig"
)

const (
	// The SigV4 credential scope. The gateway routes on the scope rather than
	// the path, so this is what selects the RDS surface.
	signingService = "rds"
	// The gateway ignores the version, but real RDS rejects a request without
	// one and this client stays pointable there.
	apiVersion = "2014-10-31"
	// Room for the command long poll; per-call deadlines ride the context.
	defaultTimeout = 40 * time.Second
)

// One client is reused for register/heartbeat/bootstrap/poll.
type Client struct {
	baseURL    string
	signer     *gwsign.Signer
	region     string
	httpClient *http.Client
}

// caPath optionally pins the gateway TLS CA; empty relies on the system trust
// store. region defaults to us-east-1, since SigV4 requires a non-empty one.
func New(baseURL, caPath string, signer *gwsign.Signer, region string, timeout time.Duration) (*Client, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("rdsgw: baseURL is required")
	}
	if signer == nil {
		return nil, fmt.Errorf("rdsgw: signer is required")
	}
	if region == "" {
		region = "us-east-1"
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	tlsCfg := &tls.Config{
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: tlsconfig.Curves,
	}
	if caPath != "" {
		pem, err := os.ReadFile(caPath)
		if err != nil {
			return nil, fmt.Errorf("rdsgw: read gateway CA %q: %w", caPath, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("rdsgw: gateway CA %q has no usable certificates", caPath)
		}
		tlsCfg.RootCAs = pool
	}

	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		signer:  signer,
		region:  region,
		httpClient: &http.Client{
			Timeout:   timeout,
			Transport: &http.Transport{TLSClientConfig: tlsCfg, MaxIdleConns: 2, IdleConnTimeout: 30 * time.Second},
		},
	}, nil
}

// Code is the AWS error code, so a caller branches on the failure class rather
// than matching message text.
type APIError struct {
	Action     string
	StatusCode int
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	if e.Message != "" && e.Message != e.Code {
		return fmt.Sprintf("rds %s: %s (%s, HTTP %d)", e.Action, e.Message, e.Code, e.StatusCode)
	}
	return fmt.Sprintf("rds %s: %s (HTTP %d)", e.Action, e.Code, e.StatusCode)
}

// Mirrors the IAM-style error envelope the gateway's query services return.
type errorResponse struct {
	XMLName xml.Name `xml:"ErrorResponse"`
	Error   struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	} `xml:"Error"`
}

// params carries the action's own arguments; Action and Version are set here.
// out may be nil. A non-2xx yields an *APIError. No retry; callers wrap.
func (c *Client) Call(ctx context.Context, action string, params url.Values, out any) error {
	form := make(url.Values, len(params)+2)
	maps.Copy(form, params)
	form.Set("Action", action)
	form.Set("Version", apiVersion)
	body := []byte(form.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create %s request: %w", action, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")

	sum := sha256.Sum256(body)
	if err := c.signer.Sign(req, hex.EncodeToString(sum[:]), signingService, c.region); err != nil {
		return fmt.Errorf("sign %s request: %w", action, err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send %s request: %w", action, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read %s response: %w", action, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseAPIError(action, resp.StatusCode, respBody)
	}
	if out == nil {
		return nil
	}
	if err := decodeResult(respBody, action, out); err != nil {
		return fmt.Errorf("decode %s response: %w", action, err)
	}
	return nil
}

// Scans for the <ActionResult> element rather than declaring the envelope as a
// type, since the wrapper names are per-action.
func decodeResult(body []byte, action string, out any) error {
	want := action + "Result"
	dec := xml.NewDecoder(bytes.NewReader(body))
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("response carries no <%s> element", want)
		}
		if err != nil {
			return err
		}
		if start, ok := tok.(xml.StartElement); ok && start.Name.Local == want {
			return dec.DecodeElement(out, &start)
		}
	}
}

// A body that is not the expected envelope still yields an APIError with the
// status and raw text, so a proxy failing ahead of the gateway is reported.
func parseAPIError(action string, status int, body []byte) error {
	apiErr := &APIError{Action: action, StatusCode: status}
	var envelope errorResponse
	if err := xml.Unmarshal(body, &envelope); err == nil && envelope.Error.Code != "" {
		apiErr.Code = envelope.Error.Code
		apiErr.Message = envelope.Error.Message
		return apiErr
	}
	apiErr.Code = fmt.Sprintf("HTTP%d", status)
	apiErr.Message = strings.TrimSpace(string(body))
	return apiErr
}
