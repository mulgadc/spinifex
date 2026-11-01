// Package sqsworkergw is the shared SQS worker guest's SigV4 client for the
// gateway's internal spinifex-sqs service. It sends only public values: the
// worker's identity is the signing session, and the receipt key never leaves
// the guest.
package sqsworkergw

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mulgadc/bluebottle/pkg/tlsconfig"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/relay"
)

const (
	// The SigV4 credential scope the gateway routes the internal surface on.
	signingService = "spinifex-sqs"
	apiVersion     = "2026-10-30"
	callTimeout    = 30 * time.Second
	maxResponse    = 64 << 10

	actionClaim    = "ClaimReceiptKey"
	actionRecord   = "RecordReceiptKey"
	actionPoll     = "PollWork"
	actionComplete = "CompleteWork"
	// maxPollResponse fits the gateway's 16 MiB poll batch as base64 XML.
	maxPollResponse = 32 << 20
)

// Signer signs a request in place for service and region.
type Signer interface {
	Sign(r *http.Request, payloadHash, service, region string) error
}

// Client calls the internal SQS worker actions for this worker.
type Client struct {
	baseURL    string
	region     string
	signer     Signer
	httpClient *http.Client
}

// New builds a TLS 1.3 client that trusts only caPEM, the cluster CA. An
// https URL, a Region and a CA are all required: there is no fallback.
func New(baseURL, region string, caPEM []byte, signer Signer) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("sqsworkergw: gateway URL must be an https URL")
	}
	if region == "" {
		return nil, errors.New("sqsworkergw: Region is required")
	}
	if signer == nil {
		return nil, errors.New("sqsworkergw: signer is required")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("sqsworkergw: gateway CA has no usable certificates")
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS13, CurvePreferences: tlsconfig.Curves, RootCAs: pool}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		region:  region,
		signer:  signer,
		httpClient: &http.Client{
			Timeout:   callTimeout,
			Transport: &http.Transport{TLSClientConfig: tlsCfg, MaxIdleConns: 1, IdleConnTimeout: 30 * time.Second},
			// A redirect would resend a signed request somewhere else.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// APIError is a gateway refusal; Code is the AWS error code.
type APIError struct {
	Action     string
	StatusCode int
	Code       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("spinifex-sqs %s: %s (HTTP %d)", e.Action, e.Code, e.StatusCode)
}

// Retryable reports a refusal that a later identical call may pass:
// contention or a store that is not yet available. Every other refusal,
// including AccessDenied, is final for this boot.
func (e *APIError) Retryable() bool {
	return e.Code == "ServiceUnavailable" || e.Code == "Throttling" || e.StatusCode == http.StatusServiceUnavailable
}

// Pending reports an AccessDenied, which a worker that has started but is
// not yet bound to its identity legitimately receives. It authorizes
// nothing; the caller may ask again within its own bound.
func (e *APIError) Pending() bool { return e.Code == "AccessDenied" }

// TransientError is a call that may not have reached the gateway, or whose
// reply was lost: signing, sending or reading failed. Both actions are safe
// to repeat, so it is retryable.
type TransientError struct{ Err error }

func (e *TransientError) Error() string   { return e.Err.Error() }
func (e *TransientError) Unwrap() error   { return e.Err }
func (e *TransientError) Retryable() bool { return true }

// ReceiptKey is the worker volume's receipt key initialization as the
// gateway reports it: pending authorizes creating or resuming one, otherwise
// Fingerprint names the key the disk must already hold.
type ReceiptKey struct {
	Pending     bool
	Fingerprint string
	// HostNodes is the gateway's declared cluster node count; a gateway
	// that does not report one reads as zero.
	HostNodes int
}

type claimResult struct {
	Pending     bool   `xml:"Pending"`
	Fingerprint string `xml:"Fingerprint"`
	HostNodes   int    `xml:"HostNodes"`
}

// ClaimReceiptKey asks for the receipt key decision. A reply that is neither
// pending nor one valid fingerprint is refused here.
func (c *Client) ClaimReceiptKey(ctx context.Context) (ReceiptKey, error) {
	var res claimResult
	if err := c.call(ctx, actionClaim, nil, &res); err != nil {
		return ReceiptKey{}, err
	}
	if res.Pending == (res.Fingerprint != "") || (res.Fingerprint != "" && !ValidFingerprint(res.Fingerprint)) {
		return ReceiptKey{}, errors.New("spinifex-sqs ClaimReceiptKey: response is not a valid decision")
	}
	return ReceiptKey(res), nil
}

// RecordReceiptKey records the key's fingerprint. Recording the same one
// again succeeds, so a lost reply is safe to repeat.
func (c *Client) RecordReceiptKey(ctx context.Context, fingerprint string) error {
	if !ValidFingerprint(fingerprint) {
		return errors.New("spinifex-sqs RecordReceiptKey: refusing to send a value that is not a fingerprint")
	}
	return c.call(ctx, actionRecord, url.Values{"Fingerprint": {fingerprint}}, nil)
}

// ValidFingerprint reports a lowercase hex SHA-256 fingerprint.
func ValidFingerprint(s string) bool {
	if len(s) != 2*sha256.Size || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func (c *Client) call(ctx context.Context, action string, params url.Values, out any) error {
	form := url.Values{"Action": {action}, "Version": {apiVersion}}
	maps.Copy(form, params)
	return c.do(ctx, action, "", []byte(form.Encode()), "application/x-www-form-urlencoded; charset=utf-8", maxResponse, out)
}

// do signs and sends one request. rawQuery, when set, carries the action and
// its arguments; the body hash is signed either way.
func (c *Client) do(ctx context.Context, action, rawQuery string, body []byte, contentType string, limit int64, out any) error {
	target := c.baseURL + "/"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("spinifex-sqs %s: build request: %w", action, err)
	}
	req.Header.Set("Content-Type", contentType)
	sum := sha256.Sum256(body)
	if err := c.signer.Sign(req, hex.EncodeToString(sum[:]), signingService, c.region); err != nil {
		return &TransientError{Err: fmt.Errorf("spinifex-sqs %s: sign: %w", action, err)}
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return &TransientError{Err: fmt.Errorf("spinifex-sqs %s: send: %w", action, err)}
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return &TransientError{Err: fmt.Errorf("spinifex-sqs %s: read response: %w", action, err)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseError(action, resp.StatusCode, respBody)
	}
	if out == nil {
		return nil
	}
	return decodeResult(action, respBody, out)
}

// PollWork long-polls the gateway for owner calls queued for this worker.
// An empty list is a poll that found nothing.
func (c *Client) PollWork(ctx context.Context) ([]relay.Work, error) {
	var res struct {
		Items []string `xml:"Items>member"`
	}
	if err := c.do(ctx, actionPoll, "", []byte(url.Values{"Action": {actionPoll}, "Version": {apiVersion}}.Encode()),
		"application/x-www-form-urlencoded; charset=utf-8", maxPollResponse, &res); err != nil {
		return nil, err
	}
	out := make([]relay.Work, 0, len(res.Items))
	for _, item := range res.Items {
		raw, err := base64.StdEncoding.DecodeString(item)
		if err != nil {
			return nil, errors.New("spinifex-sqs PollWork: item is not base64")
		}
		var w relay.Work
		if err := json.Unmarshal(raw, &w); err != nil || w.ID == "" || w.Op == "" {
			return nil, errors.New("spinifex-sqs PollWork: item is not a work item")
		}
		out = append(out, w)
	}
	return out, nil
}

// CompleteWork posts an encoded result for work item id and returns the
// gateway's outcome name. Any error leaves delivery unknown.
func (c *Client) CompleteWork(ctx context.Context, id string, result []byte) (string, error) {
	q := url.Values{"Action": {actionComplete}, "Version": {apiVersion}, "WorkId": {id}}
	var res struct {
		Outcome string `xml:"Outcome"`
	}
	if err := c.do(ctx, actionComplete, q.Encode(), result, "application/json", maxResponse, &res); err != nil {
		return "", err
	}
	return res.Outcome, nil
}

func decodeResult(action string, body []byte, out any) error {
	want := action + "Result"
	dec := xml.NewDecoder(bytes.NewReader(body))
	for {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("spinifex-sqs %s: response carries no <%s>", action, want)
		}
		if start, ok := tok.(xml.StartElement); ok && start.Name.Local == want {
			if err := dec.DecodeElement(out, &start); err != nil {
				return fmt.Errorf("spinifex-sqs %s: decode response: %w", action, err)
			}
			return nil
		}
	}
}

// The error body is not echoed: only the parsed code is kept.
func parseError(action string, status int, body []byte) error {
	var env struct {
		Error struct {
			Code string `xml:"Code"`
		} `xml:"Error"`
	}
	code := fmt.Sprintf("HTTP%d", status)
	if xml.Unmarshal(body, &env) == nil && env.Error.Code != "" {
		code = env.Error.Code
	}
	return &APIError{Action: action, StatusCode: status, Code: code}
}
