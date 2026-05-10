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

	"github.com/nats-io/nats.go"
)

// Sentinel errors for TLS certificate configuration failures in ConnectNATS.
// Callers can use errors.Is to detect permanent TLS errors without relying
// on error message text.
var (
	ErrCACertRead  = errors.New("failed to read CA cert")
	ErrCACertParse = errors.New("failed to parse CA cert")
)

// ErrClusterUnavailable is returned by NATS request helpers when the underlying
// connection is not currently connected. Callers (gateway, scatter-gather
// fan-out) use this to fail fast instead of waiting for per-call timeouts.
var ErrClusterUnavailable = errors.New("cluster unavailable: NATS disconnected")

// natsRetryEscalateAttempt is the attempt count past which ConnectNATSWithRetry
// promotes the per-attempt log line from slog.Warn to slog.Error (rate-limited
// to once a minute). With the 60s backoff cap, ~30 attempts corresponds to
// ~30 minutes of continuous disconnection — long enough to suspect a config
// error rather than a routine NATS restart.
const natsRetryEscalateAttempt = 30

// ConnectNATS establishes a connection to a NATS server with standard reconnect
// handling and logging. If token is non-empty, token authentication is used.
// If caCertPath is non-empty, TLS is enabled using the given CA certificate.
//
// Optional callback hooks (WithDisconnectHandler / WithReconnectHandler) wrap
// the default log lines so callers can react to connectivity changes (e.g. the
// daemon flips its cluster/standalone mode field) without losing the existing
// disconnect/reconnect log output.
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
			RootCAs: pool,
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
//
// retry-related fields tune the outer reconnect-with-backoff loop;
// callback fields wrap nats.go's connection-state handlers so callers can
// react to disconnects/reconnects without losing the default log lines.
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

// WithRetryDelay sets the initial delay between retries (doubles each attempt, capped at 10s).
func WithRetryDelay(d time.Duration) RetryOption {
	return func(c *retryConfig) { c.retryDelay = d }
}

// WithMaxRetryDelay overrides the upper bound on the exponential backoff
// (default 10s).
func WithMaxRetryDelay(d time.Duration) RetryOption {
	return func(c *retryConfig) { c.maxRetryDelay = d }
}

// WithDisconnectHandler registers an optional callback invoked after the
// default disconnect log line. Callback runs on a NATS client goroutine; keep
// it non-blocking (atomic stores, channel sends, goroutine spawns).
func WithDisconnectHandler(fn func(*nats.Conn, error)) RetryOption {
	return func(c *retryConfig) { c.onDisconnect = fn }
}

// WithReconnectHandler registers an optional callback invoked after the
// default reconnect log line. Same goroutine constraints as WithDisconnectHandler.
func WithReconnectHandler(fn func(*nats.Conn)) RetryOption {
	return func(c *retryConfig) { c.onReconnect = fn }
}

// WithAttemptErrHandler registers an optional callback invoked after each
// failed connect attempt during ConnectNATSWithRetry's outer loop. Used by the
// daemon to surface initial-connect retries as a counter on /local/status.
func WithAttemptErrHandler(fn func(err error, attempt int)) RetryOption {
	return func(c *retryConfig) { c.onAttemptErr = fn }
}

// WithContext lets callers cancel the retry loop. When ctx is done,
// ConnectNATSWithRetry returns ctx.Err().
func WithContext(ctx context.Context) RetryOption {
	return func(c *retryConfig) { c.ctx = ctx }
}

// ConnectNATSWithRetry calls ConnectNATS in a retry loop with exponential
// backoff. It retries for up to 5 minutes (default) before giving up; pass
// WithMaxWait(0) to retry indefinitely (cancel via WithContext). TLS
// configuration errors (ErrCACertRead, ErrCACertParse) are permanent and
// cause an immediate return without retrying.
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

		// Past the retry-escalation threshold (~30 attempts ≈ ~30 min disconnected
		// at the 60s backoff cap) the warn-on-every-retry pattern can hide a
		// stuck NATS config in routine warning noise. Escalate to slog.Error,
		// rate-limited to once a minute, so the operator's error log surfaces it.
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

// NATSRequest performs a NATS request-response with JSON marshaling.
// It marshals the input, sends to the given subject with the X-Account-ID
// header, validates the response for error payloads, and unmarshals the
// successful response into Out. Handlers can ignore the account ID if the
// operation is unscoped (e.g. DescribeInstanceTypes).
func NATSRequest[Out any](conn *nats.Conn, subject string, input any, timeout time.Duration, accountID string) (*Out, error) {
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

const (
	// maxScatterGatherResponseSize is the maximum allowed size for a single
	// scatter-gather response payload (10 MB). Responses exceeding this are
	// skipped to prevent OOM from buggy nodes.
	maxScatterGatherResponseSize = 10 * 1024 * 1024

	// maxScatterGatherUnboundedResponses is the hard cap on responses collected
	// when expectedNodes is 0 (unbounded fan-out).
	maxScatterGatherUnboundedResponses = 256
)

// NATSScatterGather publishes a fan-out request and collects responses from
// multiple nodes. It returns the first successful (non-error) response. Error
// payloads from individual nodes are skipped. If all responses are errors, the
// last error is returned. If no responses arrive before the deadline, a timeout
// error is returned. When expectedNodes > 0, collection exits early once that
// many responses have been received.
func NATSScatterGather[Out any](conn *nats.Conn, subject string, input any, timeout time.Duration, expectedNodes int, accountID string) (*Out, error) {
	if conn == nil || !conn.IsConnected() {
		return nil, ErrClusterUnavailable
	}

	jsonData, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal input: %w", err)
	}

	inbox := nats.NewInbox()
	sub, err := conn.SubscribeSync(inbox)
	if err != nil {
		return nil, fmt.Errorf("failed to create inbox: %w", err)
	}
	defer sub.Unsubscribe()

	pubMsg := nats.NewMsg(subject)
	pubMsg.Reply = inbox
	pubMsg.Data = jsonData
	pubMsg.Header.Set(AccountIDHeader, accountID)
	if err := conn.PublishMsg(pubMsg); err != nil {
		return nil, fmt.Errorf("failed to publish request: %w", err)
	}

	deadline := time.Now().Add(timeout)
	responsesReceived := 0
	var lastErr error

	maxResponses := maxScatterGatherUnboundedResponses
	if expectedNodes > 0 {
		maxResponses = expectedNodes
	}

	for time.Now().Before(deadline) {
		if responsesReceived >= maxResponses {
			break
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}

		msg, err := sub.NextMsg(remaining)
		if err != nil {
			if err == nats.ErrTimeout || err == nats.ErrNoResponders {
				break
			}
			return nil, fmt.Errorf("scatter-gather receive error: %w", err)
		}

		if len(msg.Data) > maxScatterGatherResponseSize {
			slog.Warn("ScatterGather: skipping oversized response", "subject", subject, "size", len(msg.Data))
			continue
		}

		responsesReceived++

		responseError, err := ValidateErrorPayload(msg.Data)
		if err != nil {
			slog.Debug("ScatterGather: skipping error response", "code", *responseError.Code, "subject", subject)
			lastErr = errors.New(*responseError.Code)
			continue
		}

		var output Out
		if err := json.Unmarshal(msg.Data, &output); err != nil {
			slog.Debug("ScatterGather: skipping malformed response", "subject", subject, "err", err)
			lastErr = fmt.Errorf("failed to unmarshal response: %w", err)
			continue
		}

		return &output, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("scatter-gather timeout: no responses received for %s", subject)
}

// PublishEvent marshals event as JSON and publishes it to the given NATS topic.
// Errors are logged but not returned (fire-and-forget). A nil connection is a no-op.
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

// RequestEvent marshals event as JSON and sends a NATS request, waiting for a
// response. This ensures the subscriber has processed the event before the
// caller continues. Returns an error if the request times out or the responder
// reports an error.
func RequestEvent(nc *nats.Conn, topic string, event any, timeout time.Duration) error {
	if nc == nil {
		return nil
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

// AccountIDFromMsg extracts the caller's account ID from a NATS message header.
// Returns the account ID, or empty string if the header is not set.
func AccountIDFromMsg(msg *nats.Msg) string {
	if msg == nil || msg.Header == nil {
		return ""
	}
	return msg.Header.Get(AccountIDHeader)
}
