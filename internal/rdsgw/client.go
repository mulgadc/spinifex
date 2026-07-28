// Package rdsgw is the on-VM SigV4 client the rds-agent uses to reach the AWS
// gateway over HTTPS instead of connecting to NATS directly, keeping the bus
// host-internal (the gateway relays agent calls onto rds.bus.* host-side).
//
// It is the structural sibling of internal/ecsgw — an on-VM client signing with
// the instance-role credentials — over a different transport: RDS speaks the
// AWS Query protocol with XML responses (rds-v1.md D2), not JSON 1.1, so a call
// is a form-encoded Action= POST and a response is the IAM-style
// <ActionResponse><ActionResult> envelope.
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
	// signingService is the SigV4 credential scope. The gateway routes on the
	// scope rather than the path, so this is what selects the RDS surface.
	signingService = "rds"
	// apiVersion is the RDS Query API version. The gateway ignores it, but a
	// Query request without one is malformed to a real RDS endpoint, and this
	// client should stay pointable at one.
	apiVersion = "2014-10-31"
	// defaultTimeout bounds a single call when the caller asks for none. It is
	// generous enough for the command long poll, whose window the gateway caps
	// at 20s; per-call deadlines are the caller's to set on the context.
	defaultTimeout = 40 * time.Second
)

// Client posts SigV4-signed Query-protocol requests to the gateway for service
// "rds". One client is reused for register/heartbeat/bootstrap/poll.
type Client struct {
	baseURL    string
	signer     *gwsign.Signer
	region     string
	httpClient *http.Client
}

// New builds a client. signer supplies the SigV4 credentials — production
// passes a gwsign IMDS signer, which retrieves per call so rotated instance-role
// credentials take effect without a restart. caPath optionally pins the gateway
// TLS CA; empty relies on the system trust store. region defaults to us-east-1
// when empty (SigV4 requires a non-empty region).
//
// The IMDS datapath can lag VM boot by minutes, so New deliberately does not
// wait for credentials: an agent's boot-critical calls already retry, and
// failing to start here would only move the same wait somewhere less visible.
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

// APIError is a failure the gateway reported in the Query protocol's XML error
// envelope. Code is the AWS error code, so a caller branches on the failure
// class rather than matching message text.
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

// errorResponse mirrors the IAM-style error envelope the gateway's query
// services return.
type errorResponse struct {
	XMLName xml.Name `xml:"ErrorResponse"`
	Error   struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	} `xml:"Error"`
	RequestID string `xml:"RequestId"`
}

// Call signs and POSTs a Query request for action, unmarshalling the response's
// <actionResult> element into out. params carries the action's own arguments;
// Action and Version are set here. out may be nil when the caller has no use for
// the result. A non-2xx yields an *APIError. No retry; callers wrap.
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

// decodeResult reads the <ActionResult> element out of the gateway's
// <ActionResponse> envelope into out.
//
// It scans for the element rather than declaring the envelope as a type because
// the wrapper names are per-action, and it uses encoding/xml rather than the SDK
// unmarshaler because the handler output types hold plain values where the SDK
// shapes hold pointers.
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

// parseAPIError builds an *APIError from an error response body. A body that is
// not the expected envelope still yields an APIError carrying the status and the
// raw text, so a gateway failing ahead of its own error rendering (a proxy, a
// TLS terminator) is reported rather than reduced to a bare status code.
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
