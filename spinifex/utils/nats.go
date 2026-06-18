package utils

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/mulgadc/spinifex/internal/tlsconfig"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go"
)

// Sentinel errors for TLS configuration failures in ConnectNATS.
var (
	ErrCACertRead  = errors.New("failed to read CA cert")
	ErrCACertParse = errors.New("failed to parse CA cert")
)

// ErrClusterUnavailable is returned when the NATS connection is not currently connected.
var ErrClusterUnavailable = errors.New("cluster unavailable: NATS disconnected")

// natsRetryEscalateAttempt is the threshold at which retry logs escalate from Warn to Error (rate-limited to 1/min).
// ~30 attempts at the 60 s backoff cap ≈ ~30 min disconnected, suggesting config error not transient restart.
const natsRetryEscalateAttempt = 30

// ConnectNATS connects to a NATS server with reconnect handling. Supports token auth and TLS via caCertPath.
// WithDisconnectHandler/WithReconnectHandler wrap the default log lines for callers that need to react to state changes.
func ConnectNATS(host, token, caCertPath string, opts ...RetryOption) (*nats.Conn, error) {
	cfg := retryConfig{}
	for _, o := range opts {
		o(&cfg)
	}

	natsOpts := []nats.Option{
		nats.ReconnectWait(time.Second),
		nats.MaxReconnects(-1),
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			slog.Warn("NATS disconnected", "err", err)
			if cfg.onDisconnect != nil {
				cfg.onDisconnect(nc, err)
			}
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			slog.Warn("NATS reconnected", "url", nc.ConnectedUrl())
			if cfg.onReconnect != nil {
				cfg.onReconnect(nc)
			}
		}),
	}

	if token != "" {
		natsOpts = append(natsOpts, nats.Token(token))
	}

	if caCertPath != "" {
		caCert, err := os.ReadFile(caCertPath)
		if err != nil {
			return nil, fmt.Errorf("%w %s: %v", ErrCACertRead, caCertPath, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("%w from %s", ErrCACertParse, caCertPath)
		}
		natsOpts = append(natsOpts, nats.Secure(&tls.Config{
			RootCAs:          pool,
			MinVersion:       tls.VersionTLS13,
			CurvePreferences: tlsconfig.Curves,
		}))
	}

	nc, err := nats.Connect(host, natsOpts...)
	if err != nil {
		return nil, fmt.Errorf("NATS connect failed: %w", err)
	}

	slog.Debug("Connected to NATS server", "host", host)
	return nc, nil
}

// retryConfig holds parameters for ConnectNATS / ConnectNATSWithRetry.
type retryConfig struct {
	maxWait       time.Duration
	retryDelay    time.Duration
	maxRetryDelay time.Duration
	onDisconnect  func(*nats.Conn, error)
	onReconnect   func(*nats.Conn)
	onAttemptErr  func(err error, attempt int)
	ctx           context.Context
}

// RetryOption configures ConnectNATS / ConnectNATSWithRetry behavior.
type RetryOption func(*retryConfig)

// WithMaxWait sets the maximum total time to keep retrying before giving up.
func WithMaxWait(d time.Duration) RetryOption {
	return func(c *retryConfig) { c.maxWait = d }
}

// WithRetryDelay sets the initial retry delay (exponentially doubled, capped at 10 s).
func WithRetryDelay(d time.Duration) RetryOption {
	return func(c *retryConfig) { c.retryDelay = d }
}

// WithMaxRetryDelay overrides the upper bound on the exponential backoff
// (default 10s).
func WithMaxRetryDelay(d time.Duration) RetryOption {
	return func(c *retryConfig) { c.maxRetryDelay = d }
}

// WithDisconnectHandler registers a callback invoked after the default disconnect log line.
// Runs on a NATS goroutine; keep it non-blocking.
func WithDisconnectHandler(fn func(*nats.Conn, error)) RetryOption {
	return func(c *retryConfig) { c.onDisconnect = fn }
}

// WithReconnectHandler registers a callback invoked after the default reconnect log line. Same goroutine constraints as WithDisconnectHandler.
func WithReconnectHandler(fn func(*nats.Conn)) RetryOption {
	return func(c *retryConfig) { c.onReconnect = fn }
}

// WithAttemptErrHandler registers a callback invoked after each failed attempt in ConnectNATSWithRetry.
func WithAttemptErrHandler(fn func(err error, attempt int)) RetryOption {
	return func(c *retryConfig) { c.onAttemptErr = fn }
}

// WithContext lets callers cancel the retry loop. When ctx is done,
// ConnectNATSWithRetry returns ctx.Err().
func WithContext(ctx context.Context) RetryOption {
	return func(c *retryConfig) { c.ctx = ctx }
}

// ConnectNATSWithRetry calls ConnectNATS with exponential backoff, retrying up to 5 min by default.
// Pass WithMaxWait(0) to retry indefinitely (cancel via WithContext). TLS errors return immediately.
func ConnectNATSWithRetry(host, token, caCertPath string, opts ...RetryOption) (*nats.Conn, error) {
	cfg := retryConfig{
		maxWait:       5 * time.Minute,
		retryDelay:    500 * time.Millisecond,
		maxRetryDelay: 10 * time.Second,
	}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.maxRetryDelay <= 0 {
		cfg.maxRetryDelay = 10 * time.Second
	}

	start := time.Now()
	attempt := 0
	var lastEscalatedLog time.Time
	for {
		attempt++
		nc, err := ConnectNATS(host, token, caCertPath, opts...)
		if err == nil {
			if time.Since(start) > time.Second {
				slog.Info("NATS connection established", "elapsed", time.Since(start).Round(time.Second))
			}
			return nc, nil
		}

		// TLS configuration errors are permanent — retrying will not help.
		if errors.Is(err, ErrCACertRead) || errors.Is(err, ErrCACertParse) {
			return nil, fmt.Errorf("NATS TLS configuration error: %w", err)
		}

		if cfg.onAttemptErr != nil {
			cfg.onAttemptErr(err, attempt)
		}

		elapsed := time.Since(start)
		if cfg.maxWait > 0 && elapsed >= cfg.maxWait {
			return nil, fmt.Errorf("NATS connect failed after %s: %w", elapsed.Round(time.Second), err)
		}

		// Past the escalation threshold, promote from Warn to Error (rate-limited to 1/min).
		if attempt > natsRetryEscalateAttempt {
			if lastEscalatedLog.IsZero() || time.Since(lastEscalatedLog) >= time.Minute {
				slog.Error("NATS still disconnected", "error", err, "disconnected_for", elapsed.Round(time.Second), "attempt", attempt)
				lastEscalatedLog = time.Now()
			}
		} else {
			slog.Warn("NATS not ready, retrying...", "error", err, "elapsed", elapsed.Round(time.Second), "retryIn", cfg.retryDelay, "attempt", attempt)
		}

		if cfg.ctx != nil {
			select {
			case <-cfg.ctx.Done():
				return nil, fmt.Errorf("NATS connect cancelled after %s: %w", elapsed.Round(time.Second), cfg.ctx.Err())
			case <-time.After(cfg.retryDelay):
			}
		} else {
			time.Sleep(cfg.retryDelay)
		}
		cfg.retryDelay = min(cfg.retryDelay*2, cfg.maxRetryDelay)
	}
}

// AccountIDHeader is the NATS message header key used to pass the caller's
// AWS account ID from the gateway to daemon handlers.
const AccountIDHeader = "X-Account-ID"

// PrincipalARNHeader carries the caller's resolved IAM principal ARN from gateway to daemon handlers.
const PrincipalARNHeader = "X-Principal-ARN"

// NATSHeader is an extra request header passed to NATSRequest beyond the
// always-set X-Account-ID.
type NATSHeader struct{ Key, Value string }

// PrincipalARNFromMsg extracts the caller's principal ARN from a NATS message
// header. Returns "" when absent.
func PrincipalARNFromMsg(msg *nats.Msg) string {
	if msg == nil || msg.Header == nil {
		return ""
	}
	return msg.Header.Get(PrincipalARNHeader)
}

// NATSRequest performs a NATS request-response with JSON marshaling.
// Sends with X-Account-ID (plus any extra headers) and unmarshals the successful response into Out.
func NATSRequest[Out any](conn *nats.Conn, subject string, input any, timeout time.Duration, accountID string, headers ...NATSHeader) (*Out, error) {
	if conn == nil || !conn.IsConnected() {
		return nil, ErrClusterUnavailable
	}

	jsonData, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal input: %w", err)
	}

	reqMsg := nats.NewMsg(subject)
	reqMsg.Data = jsonData
	reqMsg.Header.Set(AccountIDHeader, accountID)
	for _, h := range headers {
		if h.Key != "" {
			reqMsg.Header.Set(h.Key, h.Value)
		}
	}

	msg, err := conn.RequestMsg(reqMsg, timeout)
	if err != nil {
		if errors.Is(err, nats.ErrNoResponders) {
			return nil, fmt.Errorf("NATS request to %s: %w", subject, nats.ErrNoResponders)
		}
		return nil, fmt.Errorf("NATS request failed: %w", err)
	}

	responseError, err := ValidateErrorPayload(msg.Data)
	if err != nil {
		return nil, errors.New(*responseError.Code)
	}

	var output Out
	if err := json.Unmarshal(msg.Data, &output); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return &output, nil
}

// ServeNATSRequest unmarshals the request into *I, invokes fn, and replies with JSON or an awserrors envelope.
func ServeNATSRequest[I any, O any](msg *nats.Msg, fn func(*I) (*O, error)) {
	input := new(I)
	if errResp := UnmarshalJsonPayload(input, msg.Data); errResp != nil {
		respondNATS(msg, errResp)
		return
	}
	out, err := fn(input)
	if err != nil {
		respondNATS(msg, GenerateErrorPayload(awserrors.ValidErrorCode(err.Error())))
		return
	}
	data, err := json.Marshal(out)
	if err != nil {
		respondNATS(msg, GenerateErrorPayload(awserrors.ErrorServerInternal))
		return
	}
	respondNATS(msg, data)
}

func respondNATS(msg *nats.Msg, data []byte) {
	if err := msg.Respond(data); err != nil {
		slog.Error("failed to respond to NATS request", "subject", msg.Subject, "err", err)
	}
}

const (
	// maxScatterGatherResponseSize caps a single scatter-gather response at 10 MB to prevent OOM.
	maxScatterGatherResponseSize = 10 * 1024 * 1024
	// maxScatterGatherUnboundedResponses caps responses when expectedNodes is 0.
	maxScatterGatherUnboundedResponses = 256
)

// Summary is a local tally of a fan-out; it is never sent over the wire.
type Summary struct {
	Received       int            // frames seen (success + error)
	Successes      int            // frames that decoded as a non-error envelope
	ErrorCodes     map[string]int // AWS error code -> count across error frames
	FirstClient4xx string         // first deterministic 4xx code seen, "" if none
	TimedOut       bool           // deadline hit before the stop condition was met
}

// GatherOpts configures a Gather fan-out.
type GatherOpts struct {
	Timeout       time.Duration // hard deadline for the whole fan-out
	ExpectedNodes int           // early-exit once this many frames arrive (0 = wait full Timeout)
	StopOnFirst   bool          // return after the first non-error frame (first-wins)
	AccountID     string        // sets X-Account-ID header when non-empty
}

// Gather publishes payload to subject over a fresh inbox and collects reply frames
// until ExpectedNodes answer, StopOnFirst yields a success, or Timeout elapses.
// Error envelopes and oversized frames are dropped from frames but counted in sum;
// returned frames are raw daemon replies for the caller to decode and merge.
func Gather(conn *nats.Conn, subject string, payload []byte, opts GatherOpts) (frames [][]byte, sum Summary, err error) {
	sum.ErrorCodes = map[string]int{}
	if conn == nil || !conn.IsConnected() {
		return nil, sum, ErrClusterUnavailable
	}

	inbox := nats.NewInbox()
	sub, err := conn.SubscribeSync(inbox)
	if err != nil {
		return nil, sum, fmt.Errorf("failed to create inbox: %w", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	pubMsg := nats.NewMsg(subject)
	pubMsg.Reply = inbox
	pubMsg.Data = payload
	if opts.AccountID != "" {
		pubMsg.Header.Set(AccountIDHeader, opts.AccountID)
	}
	if err := conn.PublishMsg(pubMsg); err != nil {
		return nil, sum, fmt.Errorf("failed to publish request: %w", err)
	}

	maxResponses := maxScatterGatherUnboundedResponses
	if opts.ExpectedNodes > 0 {
		maxResponses = opts.ExpectedNodes
	}

	deadline := time.Now().Add(opts.Timeout)
	for sum.Received < maxResponses {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			sum.TimedOut = true
			break
		}

		msg, nerr := sub.NextMsg(remaining)
		if nerr != nil {
			if errors.Is(nerr, nats.ErrTimeout) || errors.Is(nerr, nats.ErrNoResponders) {
				sum.TimedOut = true
				break
			}
			return frames, sum, fmt.Errorf("gather receive error on %s: %w", subject, nerr)
		}

		sum.Received++

		if len(msg.Data) > maxScatterGatherResponseSize {
			slog.Warn("Gather: skipping oversized response", "subject", subject, "size", len(msg.Data))
			continue
		}

		responseError, verr := ValidateErrorPayload(msg.Data)
		if verr != nil {
			code := ""
			if responseError.Code != nil {
				code = *responseError.Code
			}
			sum.ErrorCodes[code]++
			// Capture the first deterministic 4xx; callers propagate it only when nothing was collected.
			if sum.FirstClient4xx == "" && code != "" {
				if info, known := awserrors.ErrorLookup[code]; known && info.HTTPCode >= 400 && info.HTTPCode < 500 {
					sum.FirstClient4xx = code
				}
			}
			slog.Debug("Gather: skipping error response", "code", code, "subject", subject)
			continue
		}

		sum.Successes++
		frames = append(frames, msg.Data)
		if opts.StopOnFirst {
			return frames, sum, nil
		}
	}

	return frames, sum, nil
}

// PublishEvent marshals event as JSON and publishes to topic (fire-and-forget; nil conn is a no-op).
func PublishEvent(nc *nats.Conn, topic string, event any) {
	if nc == nil {
		return
	}
	data, err := json.Marshal(event)
	if err != nil {
		slog.Warn("Failed to marshal event", "topic", topic, "error", err)
		return
	}
	if err := nc.Publish(topic, data); err != nil {
		slog.Warn("Failed to publish event", "topic", topic, "error", err)
	}
}

// natEvent is the wire payload for vpc.add-nat / vpc.delete-nat.
type natEvent struct {
	VpcId      string `json:"vpc_id"`
	ExternalIP string `json:"external_ip"`
	LogicalIP  string `json:"logical_ip"`
	PortName   string `json:"port_name"`
	MAC        string `json:"mac"`
}

// AddNAT requests vpcd commit the OVN dnat_and_snat rule via NATS request-reply (10 s timeout).
// A non-nil return means the rule may not be committed; callers must roll back and publish vpc.delete-nat.
func AddNAT(nc *nats.Conn, vpcID, externalIP, logicalIP, portName, mac string) error {
	return RequestEvent(nc, "vpc.add-nat", natEvent{
		VpcId: vpcID, ExternalIP: externalIP, LogicalIP: logicalIP,
		PortName: portName, MAC: mac,
	}, 10*time.Second)
}

// PublishNATEvent sends a NAT lifecycle event. vpc.add-nat uses request-reply (prevents ARP races);
// vpc.delete-nat is fire-and-forget. Use AddNAT directly when failure must trigger a rollback.
func PublishNATEvent(nc *nats.Conn, topic, vpcID, externalIP, logicalIP, portName, mac string) {
	evt := natEvent{
		VpcId: vpcID, ExternalIP: externalIP, LogicalIP: logicalIP,
		PortName: portName, MAC: mac,
	}

	if topic == "vpc.add-nat" {
		if err := RequestEvent(nc, topic, evt, 10*time.Second); err != nil {
			slog.Warn("PublishNATEvent: failed to add NAT rule — OVN dnat_and_snat rule not created; restart vpcd or re-associate EIP to recover",
				"topic", topic, "externalIP", externalIP, "logicalIP", logicalIP, "err", err)
		}
		return
	}
	PublishEvent(nc, topic, evt)
}

// RequestEvent marshals event as JSON and sends a synchronous NATS request, blocking until the subscriber acks.
func RequestEvent(nc *nats.Conn, topic string, event any, timeout time.Duration) error {
	if nc == nil {
		return fmt.Errorf("%s: nats connection not initialized", topic)
	}
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal %s event: %w", topic, err)
	}
	resp, err := nc.Request(topic, data, timeout)
	if err != nil {
		return fmt.Errorf("%s request: %w", topic, err)
	}
	// vpcd responds with {"success":true} or {"success":false,"error":"..."}.
	var result struct {
		Success bool   `json:"success"`
		Error   string `json:"error,omitempty"`
	}
	if jsonErr := json.Unmarshal(resp.Data, &result); jsonErr != nil {
		return fmt.Errorf("%s: unmarshal response: %w", topic, jsonErr)
	}
	if !result.Success {
		return fmt.Errorf("%s: %s", topic, result.Error)
	}
	return nil
}

// AccountIDFromMsg extracts the caller's account ID from a NATS message header, or "" if absent.
func AccountIDFromMsg(msg *nats.Msg) string {
	if msg == nil || msg.Header == nil {
		return ""
	}
	return msg.Header.Get(AccountIDHeader)
}
