// Package latitude is the narrow slice of the latitude.sh API that floating
// public addresses need: reading a project's Elastic IPs and moving one to
// another server.
//
// It is a direct net/http client, like the Vultr one. Request and response
// shapes follow the published OpenAPI spec and the Terraform provider; no call
// has been made against the live API, so the tests use bodies written from the
// spec, not captured responses.
package latitude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

// DefaultBaseURL is the public API.
const DefaultBaseURL = "https://api.latitude.sh"

// DefaultKeyFile holds the node's API token, readable by the daemon's user only.
const DefaultKeyFile = "/etc/spinifex/latitude/api-token"

// DefaultCallTimeout bounds one HTTP round trip.
const DefaultCallTimeout = 15 * time.Second

const (
	defaultAttempts = 3
	// DefaultMoveTimeout bounds how long a move waits for the address to be active again.
	DefaultMoveTimeout = 2 * time.Minute
	// DefaultPollInterval is the gap between status reads while a move settles.
	DefaultPollInterval = 3 * time.Second
)

var (
	// ErrNotFound means the Elastic IP or server does not exist.
	ErrNotFound = errors.New("latitude: resource not found")
	// ErrMoveRefused means the API will not move the address to that server:
	// another project, another site, or an incompatible network. Retrying is pointless.
	ErrMoveRefused = errors.New("latitude: move refused")
	// ErrNotActive means the address is still settling from an earlier change, so no move
	// was attempted; the caller tries again on its next pass.
	ErrNotActive = errors.New("latitude: Elastic IP not active")
	// ErrNotSettled means the address did not return to active within the move timeout.
	ErrNotSettled = errors.New("latitude: Elastic IP did not become active")
	// ErrUnauthorized means the token was refused.
	ErrUnauthorized = errors.New("latitude: API token refused")
	// ErrUnavailable means the API was throttled or failing; retry later.
	ErrUnavailable = errors.New("latitude: API unavailable")
)

// APIError is a non-2xx response, carrying latitude.sh's own code and message.
type APIError struct {
	Status  int
	Code    string
	Message string
	kind    error
}

// Error reports the status and, when it is a well-formed code, latitude.sh's error code.
// The response body is never included: an upstream error can echo a credential.
func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("latitude: HTTP %d %s", e.Status, e.Code)
	}
	return fmt.Sprintf("latitude: HTTP %d %s", e.Status, e.Message)
}

// Unwrap exposes the classified sentinel, if any, to errors.Is.
func (e *APIError) Unwrap() error { return e.kind }

// Status values of an Elastic IP.
const (
	StatusActive = "active"
	StatusError  = "error"
)

// ElasticIP is one Elastic IP.
type ElasticIP struct {
	ID           string
	Address      netip.Addr
	Status       string
	Mode         string
	Family       string
	PrefixLength int
	// ServerID is empty when the address is unassigned or its server is not
	// active, which the API does not distinguish.
	ServerID string
}

// Unusable returns why the address cannot be handed out by a floating pool, or
// empty. Only a routed IPv4 /32 can be moved between servers.
func (e ElasticIP) Unusable() string {
	if e.Mode != "routed" || e.Family != "IPv4" || e.PrefixLength != 32 {
		return fmt.Sprintf("mode %s, family %s, prefix length %d: only a routed IPv4 /32 can be moved",
			safe(e.Mode, "routed", "bgp"), safe(e.Family, "IPv4", "IPv6"), e.PrefixLength)
	}
	return ""
}

// Config selects the endpoint, credential and project.
type Config struct {
	BaseURL     string
	KeyFile     string
	ProjectID   string
	CallTimeout time.Duration
	HTTPClient  *http.Client
	// Attempts is the total tries for a retryable failure; zero means the default.
	Attempts int
	// Backoff is the first retry delay; it doubles with jitter. Zero means 500ms.
	Backoff time.Duration
	// MoveTimeout and PollInterval pace the wait after a move; zero means the defaults.
	MoveTimeout  time.Duration
	PollInterval time.Duration
}

// Client implements the Elastic IP calls against one project.
type Client struct {
	cfg  Config
	http *http.Client
}

// New builds a client. It makes no call; ListElasticIPs proves the token works.
func New(cfg Config) (*Client, error) {
	if cfg.ProjectID == "" {
		return nil, errors.New("latitude: project id is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.KeyFile == "" {
		cfg.KeyFile = DefaultKeyFile
	}
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = DefaultCallTimeout
	}
	if cfg.Attempts <= 0 {
		cfg.Attempts = defaultAttempts
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = 500 * time.Millisecond
	}
	if cfg.MoveTimeout <= 0 {
		cfg.MoveTimeout = DefaultMoveTimeout
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	hc := &http.Client{}
	if cfg.HTTPClient != nil {
		clone := *cfg.HTTPClient
		hc = &clone
	}
	// The API never redirects a call we make. Following a 307 or 308 would replay a
	// PATCH body, a second write the caller did not authorise.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{cfg: cfg, http: hc}, nil
}

type elasticIPJSON struct {
	ID         *string `json:"id"`
	Attributes struct {
		Address      string `json:"address"`
		Status       string `json:"status"`
		Mode         string `json:"mode"`
		Family       string `json:"family"`
		PrefixLength int    `json:"prefix_length"`
		Server       *struct {
			ID string `json:"id"`
		} `json:"server"`
	} `json:"attributes"`
}

// toElasticIP keeps every ID exactly as the API sent it. IDs are opaque: they are compared, never
// inspected, and the caller decides which may be logged.
func (r elasticIPJSON) toElasticIP() (ElasticIP, error) {
	if r.ID == nil || *r.ID == "" {
		return ElasticIP{}, errors.New("latitude: Elastic IP with no id in response")
	}
	addr, err := netip.ParseAddr(r.Attributes.Address)
	if err != nil {
		return ElasticIP{}, errors.New("latitude: Elastic IP response has an unparseable address")
	}
	out := ElasticIP{
		ID: *r.ID, Address: addr, Status: r.Attributes.Status,
		Mode: r.Attributes.Mode, Family: r.Attributes.Family, PrefixLength: r.Attributes.PrefixLength,
	}
	if r.Attributes.Server != nil {
		if r.Attributes.Server.ID != "" {
			out.ServerID = r.Attributes.Server.ID
		}
	}
	return out, nil
}

// GetElasticIP reads one address, including which server holds it.
func (c *Client) GetElasticIP(ctx context.Context, id string) (ElasticIP, error) {
	var out struct {
		Data elasticIPJSON `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/elastic_ips/"+url.PathEscape(id), nil, &out); err != nil {
		return ElasticIP{}, err
	}
	return out.Data.toElasticIP()
}

// MoveElasticIP points id at serverID and waits until it is active there.
// It makes at most one write per call: the caller checked that it still owns the
// address just before calling, and a second write after a wait would outlive that
// check. An address that is not active is reported as ErrNotActive without a
// write, and the caller tries again on its next pass. After the write only reads
// follow, until the move settles or the move timeout passes.
func (c *Client) MoveElasticIP(ctx context.Context, id, serverID string) error {
	cur, err := c.GetElasticIP(ctx, id)
	if err != nil {
		return err
	}
	if why := cur.Unusable(); why != "" {
		return fmt.Errorf("%w: %s: %s", ErrMoveRefused, id, why)
	}
	switch {
	case cur.Status == StatusError:
		return fmt.Errorf("%w: %s is in the error state", ErrMoveRefused, id)
	case cur.Status == StatusActive && cur.ServerID == serverID:
		return nil
	case cur.Status != StatusActive:
		return fmt.Errorf("%w: %s is %s", ErrNotActive, id, safe(cur.Status, "pending", "configuring", "moving", "releasing"))
	}
	body := map[string]any{"data": map[string]any{
		"type":       "elastic_ips",
		"attributes": map[string]string{"server_id": serverID},
	}}
	// Exactly one attempt: a retry after a throttle, a 5xx or a lost connection would be a second
	// write after the caller's ownership check, and the next engine pass revalidates anyway.
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("latitude: encode move: %w", err)
	}
	if err := c.once(ctx, http.MethodPatch, "/elastic_ips/"+url.PathEscape(id), payload, nil); err != nil {
		return err
	}
	return c.awaitActive(ctx, id, serverID)
}

// awaitActive reads the address until it is active on serverID. It writes nothing.
func (c *Client) awaitActive(ctx context.Context, id, serverID string) error {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.MoveTimeout)
	defer cancel()
	for {
		cur, err := c.GetElasticIP(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("%w: %s: %w", ErrNotSettled, id, err)
			}
			return err
		}
		if cur.Status == StatusError {
			return fmt.Errorf("%w: %s went to the error state", ErrMoveRefused, id)
		}
		if cur.Status == StatusActive && cur.ServerID == serverID {
			return nil
		}
		if err := c.pause(ctx); err != nil {
			return fmt.Errorf("%w: %s: %w", ErrNotSettled, id, err)
		}
	}
}

func (c *Client) pause(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(c.cfg.PollInterval):
		return nil
	}
}

// do runs one call, retrying throttles and server failures. Writes are retried
// too: a repeated move to the same server is harmless.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var payload []byte
	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return fmt.Errorf("latitude: encode %s %s: %w", method, path, err)
		}
	}
	delay := c.cfg.Backoff
	for attempt := 1; ; attempt++ {
		err := c.once(ctx, method, path, payload, out)
		if err == nil || !errors.Is(err, ErrUnavailable) || attempt >= c.cfg.Attempts {
			return err
		}
		wait := delay/2 + rand.N(delay) //#nosec G404 - backoff jitter, not a secret
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(wait):
		}
		delay *= 2
	}
}

// once makes a single round trip. The token is read on every call, so rotating
// it in place needs no restart.
func (c *Client) once(ctx context.Context, method, path string, payload []byte, out any) error {
	key, err := c.key()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.CallTimeout)
	defer cancel()

	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.BaseURL+path, body)
	if err != nil {
		return fmt.Errorf("latitude: build %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s %s: %s", ErrUnavailable, method, path, redact(err.Error(), key))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%w: read %s %s: %s", ErrUnavailable, method, path, redact(err.Error(), key))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return classify(resp.StatusCode, raw)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("latitude: decode %s %s: %s", method, path, redact(err.Error(), key))
	}
	return nil
}

func (c *Client) key() (string, error) {
	raw, err := os.ReadFile(c.cfg.KeyFile)
	if err != nil {
		return "", fmt.Errorf("latitude: read API token: %w", err)
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return "", fmt.Errorf("latitude: API token file %s is empty", c.cfg.KeyFile)
	}
	return key, nil
}

// knownCodes are the error codes latitude.sh documents for Elastic IPs. Any other
// code is dropped, so a response cannot put its own text into an error.
var knownCodes = map[string]struct{}{
	"ELASTIC_IP_NOT_ACTIVE":        {},
	"ELASTIC_IP_NOT_RELEASABLE":    {},
	"ELASTIC_IP_LIMIT_REACHED":     {},
	"ELASTIC_IP_HAS_BGP_SESSIONS":  {},
	"IP_ALLOCATION_FAILED":         {},
	"SERVER_NOT_IN_PROJECT":        {},
	"SITE_MISMATCH":                {},
	"SITE_NOT_SUPPORTED":           {},
	"SERVER_NETWORK_INCOMPATIBLE":  {},
	"BGP_ELASTIC_IP_NOT_UPDATABLE": {},
}

// safe returns v when it is one of the documented values, else "unknown". Values
// read from a response never reach an error otherwise.
func safe(v string, known ...string) string {
	if slices.Contains(known, v) {
		return v
	}
	return "unknown"
}

// classify maps a non-2xx response onto the sentinels callers branch on. Only the
// status and a well-formed error code are kept; the body itself never reaches an error.
func classify(status int, raw []byte) error {
	var body struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(raw, &body)
	e := &APIError{Status: status, Message: http.StatusText(status)}
	if len(body.Errors) > 0 {
		if _, ok := knownCodes[body.Errors[0].Code]; ok {
			e.Code = body.Errors[0].Code
		}
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		e.kind = ErrUnauthorized
	case status == http.StatusNotFound:
		e.kind = ErrNotFound
	case status == http.StatusTooManyRequests || status >= 500:
		e.kind = ErrUnavailable
	case e.Code == "ELASTIC_IP_NOT_ACTIVE":
		e.kind = ErrNotActive
	case status == http.StatusUnprocessableEntity:
		switch e.Code {
		case "SERVER_NOT_IN_PROJECT", "SITE_MISMATCH", "SITE_NOT_SUPPORTED", "SERVER_NETWORK_INCOMPATIBLE", "BGP_ELASTIC_IP_NOT_UPDATABLE":
			e.kind = ErrMoveRefused
		}
	}
	return e
}

// redact removes the token from a message that might carry it.
func redact(msg, token string) string {
	if token == "" {
		return msg
	}
	return strings.ReplaceAll(msg, token, "[redacted]")
}
