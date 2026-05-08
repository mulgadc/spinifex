package utils

import (
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

// ConnectNATS establishes a connection to a NATS server with standard reconnect
// handling and logging. If token is non-empty, token authentication is used.
// If caCertPath is non-empty, TLS is enabled using the given CA certificate.
func ConnectNATS(host, token, caCertPath string) (*nats.Conn, error) {
	opts := []nats.Option{
		nats.ReconnectWait(time.Second),
		nats.MaxReconnects(-1),
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			slog.Warn("NATS disconnected", "err", err)
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			slog.Warn("NATS reconnected", "url", nc.ConnectedUrl())
		}),
	}

	if token != "" {
		opts = append(opts, nats.Token(token))
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
		opts = append(opts, nats.Secure(&tls.Config{
			RootCAs: pool,
		}))
	}

	nc, err := nats.Connect(host, opts...)
	if err != nil {
		return nil, fmt.Errorf("NATS connect failed: %w", err)
	}

	slog.Debug("Connected to NATS server", "host", host)
	return nc, nil
}

// retryConfig holds parameters for ConnectNATSWithRetry.
type retryConfig struct {
	maxWait    time.Duration
	retryDelay time.Duration
}

// RetryOption configures ConnectNATSWithRetry behavior.
type RetryOption func(*retryConfig)

// WithMaxWait sets the maximum total time to keep retrying before giving up.
func WithMaxWait(d time.Duration) RetryOption {
	return func(c *retryConfig) { c.maxWait = d }
}

// WithRetryDelay sets the initial delay between retries (doubles each attempt, capped at 10s).
func WithRetryDelay(d time.Duration) RetryOption {
	return func(c *retryConfig) { c.retryDelay = d }
}

// ConnectNATSWithRetry calls ConnectNATS in a retry loop with exponential
// backoff. It retries for up to 5 minutes (default) before giving up. TLS
// configuration errors (ErrCACertRead, ErrCACertParse) are permanent and
// cause an immediate return without retrying.
func ConnectNATSWithRetry(host, token, caCertPath string, opts ...RetryOption) (*nats.Conn, error) {
	cfg := retryConfig{
		maxWait:    5 * time.Minute,
		retryDelay: 500 * time.Millisecond,
	}
	for _, o := range opts {
		o(&cfg)
	}

	start := time.Now()
	for {
		nc, err := ConnectNATS(host, token, caCertPath)
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

		elapsed := time.Since(start)
		if elapsed >= cfg.maxWait {
			return nil, fmt.Errorf("NATS connect failed after %s: %w", elapsed.Round(time.Second), err)
		}

		slog.Warn("NATS not ready, retrying...", "error", err, "elapsed", elapsed.Round(time.Second), "retryIn", cfg.retryDelay)
		time.Sleep(cfg.retryDelay)
		cfg.retryDelay = min(cfg.retryDelay*2, 10*time.Second)
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

// AccountIDFromMsg extracts the caller's account ID from a NATS message header.
// Returns the account ID, or empty string if the header is not set.
func AccountIDFromMsg(msg *nats.Msg) string {
	if msg == nil || msg.Header == nil {
		return ""
	}
	return msg.Header.Get(AccountIDHeader)
}
