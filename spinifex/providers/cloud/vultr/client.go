// Package vultr is the narrow slice of the Vultr v2 API that floating public
// addresses need: reserved IPs and the instance VPC listing formation reads.
//
// It is a direct net/http client rather than govultr or vultr-cli. The surface
// is seven calls with bearer auth, so a client this size costs less than a
// dependency, needs no host binary, and is tested against captured responses.
package vultr

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
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the public v2 API.
const DefaultBaseURL = "https://api.vultr.com/v2"

// DefaultKeyFile holds the node's API key, readable by the daemon's user only.
const DefaultKeyFile = "/etc/spinifex/vultr/api-key"

// DefaultCallTimeout bounds one HTTP round trip. Allocation sits inside the
// AWS client's 60s read timeout, so a call plus its retries stays well under.
const DefaultCallTimeout = 15 * time.Second

// defaultAttempts is how many times a retryable failure is tried in total.
const defaultAttempts = 3

// listPageSize is the largest page the reserved-IP listing accepts.
const listPageSize = 500

var (
	// ErrNotFound means the reserved IP or instance does not exist.
	ErrNotFound = errors.New("vultr: resource not found")
	// ErrQuotaExceeded means the account cannot hold another reserved IP.
	ErrQuotaExceeded = errors.New("vultr: reserved IP quota exceeded")
	// ErrHolderBusy means the address is attached to a different instance.
	ErrHolderBusy = errors.New("vultr: reserved IP attached to another instance")
	// ErrUnauthorized means the key was refused, including by its IP allowlist.
	ErrUnauthorized = errors.New("vultr: API key refused")
	// ErrUnavailable means the API was throttled or failing; retry later.
	ErrUnavailable = errors.New("vultr: API unavailable")
)

// errAlreadyAttached is Vultr refusing an attach of an address that is
// attached anywhere, including to the instance named. Attach resolves which.
var errAlreadyAttached = errors.New("vultr: reserved IP already attached")

// errDetachRefused is Vultr's 400 "Unable to detach subnet" on a detach or a
// delete. Measured as transient: the same detach succeeded moments later.
var errDetachRefused = errors.New("vultr: detach refused")

// APIError is a non-2xx response, carrying Vultr's own message.
type APIError struct {
	Status  int
	Message string
	kind    error
}

func (e *APIError) Error() string {
	return fmt.Sprintf("vultr: HTTP %d: %s", e.Status, e.Message)
}

// Unwrap exposes the classified sentinel, if any, to errors.Is.
func (e *APIError) Unwrap() error { return e.kind }

// ReservedIP is one Vultr reserved IPv4 address.
type ReservedIP struct {
	ID         string
	Region     string
	Label      string
	Address    netip.Addr
	InstanceID string // empty when unattached
}

// VPCAttachment is one VPC an instance is attached to, with its address there.
type VPCAttachment struct {
	VPCID   string
	MAC     string
	Address netip.Addr
}

// Config selects the endpoint, credential and region.
type Config struct {
	BaseURL     string
	KeyFile     string
	Region      string
	CallTimeout time.Duration
	HTTPClient  *http.Client
	// Attempts is the total tries for a retryable failure; zero means the default.
	Attempts int
	// Backoff is the first retry delay; it doubles with jitter. Zero means 500ms.
	Backoff time.Duration
}

// Client implements the reserved-IP calls against one region.
type Client struct {
	cfg  Config
	http *http.Client
}

// New builds a client. It makes no call; ListReservedIPs proves the key works.
func New(cfg Config) (*Client, error) {
	if cfg.Region == "" {
		return nil, errors.New("vultr: region is required")
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
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{cfg: cfg, http: hc}, nil
}

// Region is the Vultr region every address is created in.
func (c *Client) Region() string { return c.cfg.Region }

type reservedIPJSON struct {
	ID         string `json:"id"`
	Region     string `json:"region"`
	IPType     string `json:"ip_type"`
	Subnet     string `json:"subnet"`
	SubnetSize int    `json:"subnet_size"`
	Label      string `json:"label"`
	InstanceID string `json:"instance_id"`
}

func (r reservedIPJSON) toReservedIP() (ReservedIP, error) {
	if r.ID == "" {
		return ReservedIP{}, errors.New("vultr: reserved IP with no id in response")
	}
	addr, err := netip.ParseAddr(r.Subnet)
	if err != nil {
		return ReservedIP{}, fmt.Errorf("vultr: reserved IP %s has unparseable address %q: %w", r.ID, r.Subnet, err)
	}
	return ReservedIP{ID: r.ID, Region: r.Region, Label: r.Label, Address: addr, InstanceID: r.InstanceID}, nil
}

// CreateReservedIP creates an unattached IPv4 address carrying label.
//
// Not retried on a 5xx: Vultr may have created the address before failing,
// and a retry would make a second. The label lets reconcile collect either.
func (c *Client) CreateReservedIP(ctx context.Context, label string) (ReservedIP, error) {
	body := map[string]string{"region": c.cfg.Region, "ip_type": "v4", "label": label}
	var out struct {
		ReservedIP reservedIPJSON `json:"reserved_ip"`
	}
	if err := c.do(ctx, http.MethodPost, "/reserved-ips", body, &out, false); err != nil {
		return ReservedIP{}, err
	}
	return out.ReservedIP.toReservedIP()
}

// GetReservedIP reads one address, including which instance holds it.
func (c *Client) GetReservedIP(ctx context.Context, id string) (ReservedIP, error) {
	var out struct {
		ReservedIP reservedIPJSON `json:"reserved_ip"`
	}
	if err := c.do(ctx, http.MethodGet, "/reserved-ips/"+url.PathEscape(id), nil, &out, true); err != nil {
		return ReservedIP{}, err
	}
	return out.ReservedIP.toReservedIP()
}

// ListReservedIPs returns every address in this client's region, ours or not.
func (c *Client) ListReservedIPs(ctx context.Context) ([]ReservedIP, error) {
	var all []ReservedIP
	cursor := ""
	for {
		q := url.Values{"per_page": {strconv.Itoa(listPageSize)}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var out struct {
			ReservedIPs []reservedIPJSON `json:"reserved_ips"`
			Meta        struct {
				Links struct {
					Next string `json:"next"`
				} `json:"links"`
			} `json:"meta"`
		}
		if err := c.do(ctx, http.MethodGet, "/reserved-ips?"+q.Encode(), nil, &out, true); err != nil {
			return nil, err
		}
		for _, r := range out.ReservedIPs {
			if r.Region != c.cfg.Region || r.IPType != "v4" {
				continue
			}
			ip, err := r.toReservedIP()
			if err != nil {
				return nil, err
			}
			all = append(all, ip)
		}
		if out.Meta.Links.Next == "" || out.Meta.Links.Next == cursor {
			return all, nil
		}
		cursor = out.Meta.Links.Next
	}
}

// AttachReservedIP attaches id to instanceID. It succeeds when the address is
// already attached there, which Vultr itself refuses as a 400, and reports
// ErrHolderBusy when another instance holds it.
func (c *Client) AttachReservedIP(ctx context.Context, id, instanceID string) error {
	body := map[string]string{"instance_id": instanceID}
	err := c.do(ctx, http.MethodPost, "/reserved-ips/"+url.PathEscape(id)+"/attach", body, nil, true)
	if !errors.Is(err, errAlreadyAttached) {
		return err
	}
	cur, gerr := c.GetReservedIP(ctx, id)
	if gerr != nil {
		return errors.Join(err, gerr)
	}
	if cur.InstanceID == instanceID {
		return nil
	}
	return fmt.Errorf("%w: %s is attached to %s", ErrHolderBusy, id, cur.InstanceID)
}

// DetachReservedIP detaches id from whichever instance holds it. Vultr answers
// 204 for an address that is not attached, so this is idempotent.
func (c *Client) DetachReservedIP(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/reserved-ips/"+url.PathEscape(id)+"/detach", nil, nil, true)
}

// DeleteReservedIP releases id. Vultr detaches an attached address itself.
func (c *Client) DeleteReservedIP(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/reserved-ips/"+url.PathEscape(id), nil, nil, true)
}

// ListInstanceVPCs returns the VPCs instanceID is attached to and its address
// in each. Terraform does not report the address, so formation reads it here.
func (c *Client) ListInstanceVPCs(ctx context.Context, instanceID string) ([]VPCAttachment, error) {
	var out struct {
		VPCs []struct {
			ID         string `json:"id"`
			MACAddress string `json:"mac_address"`
			IPAddress  string `json:"ip_address"`
		} `json:"vpcs"`
	}
	if err := c.do(ctx, http.MethodGet, "/instances/"+url.PathEscape(instanceID)+"/vpcs?per_page=100", nil, &out, true); err != nil {
		return nil, err
	}
	vpcs := make([]VPCAttachment, 0, len(out.VPCs))
	for _, v := range out.VPCs {
		addr, err := netip.ParseAddr(v.IPAddress)
		if err != nil {
			return nil, fmt.Errorf("vultr: VPC %s on %s has unparseable address %q: %w", v.ID, instanceID, v.IPAddress, err)
		}
		vpcs = append(vpcs, VPCAttachment{VPCID: v.ID, MAC: v.MACAddress, Address: addr})
	}
	return vpcs, nil
}

// do runs one call, retrying throttles, server failures and a refused detach
// when retry5xx is set. A 429 is always retried, since Vultr refused it before acting.
func (c *Client) do(ctx context.Context, method, path string, in, out any, retry5xx bool) error {
	var payload []byte
	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return fmt.Errorf("vultr: encode %s %s: %w", method, path, err)
		}
	}
	delay := c.cfg.Backoff
	var err error
	for attempt := 1; ; attempt++ {
		var status int
		status, err = c.once(ctx, method, path, payload, out)
		retryable := (errors.Is(err, ErrUnavailable) || errors.Is(err, errDetachRefused)) &&
			(retry5xx || status == http.StatusTooManyRequests)
		if err == nil || !retryable || attempt >= c.cfg.Attempts {
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

// once makes a single round trip. The key is read on every call, so rotating
// it in place needs no restart. A transport failure reports status zero.
func (c *Client) once(ctx context.Context, method, path string, payload []byte, out any) (int, error) {
	key, err := c.key()
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.CallTimeout)
	defer cancel()

	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.BaseURL+path, body)
	if err != nil {
		return 0, fmt.Errorf("vultr: build %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%w: %s %s: %w", ErrUnavailable, method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("%w: read %s %s: %w", ErrUnavailable, method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, classify(resp.StatusCode, raw)
	}
	if out == nil || len(raw) == 0 {
		return resp.StatusCode, nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return resp.StatusCode, fmt.Errorf("vultr: decode %s %s: %w", method, path, err)
	}
	return resp.StatusCode, nil
}

func (c *Client) key() (string, error) {
	raw, err := os.ReadFile(c.cfg.KeyFile)
	if err != nil {
		return "", fmt.Errorf("vultr: read API key: %w", err)
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return "", fmt.Errorf("vultr: API key file %s is empty", c.cfg.KeyFile)
	}
	return key, nil
}

// classify maps a non-2xx response onto the sentinels callers branch on.
// Every case except the quota one is pinned by a captured response.
func classify(status int, raw []byte) error {
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	msg := body.Error
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	e := &APIError{Status: status, Message: msg}
	lower := strings.ToLower(msg)
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		e.kind = ErrUnauthorized
	case status == http.StatusNotFound:
		e.kind = ErrNotFound
	case status == http.StatusTooManyRequests || status >= 500:
		e.kind = ErrUnavailable
	case strings.Contains(lower, "already attached"):
		e.kind = errAlreadyAttached
	case strings.Contains(lower, "unable to detach"):
		e.kind = errDetachRefused
	case strings.Contains(lower, "limit") || strings.Contains(lower, "quota") || strings.Contains(lower, "maximum"):
		e.kind = ErrQuotaExceeded
	}
	return e
}
