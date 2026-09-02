package utils

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/mulgadc/bluebottle/pkg/tlsconfig"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/otelsetup"
	"github.com/nats-io/nats.go"
)

// Sentinel errors for TLS configuration failures in ConnectNATS.
var (
	ErrCACertRead  = errors.New("failed to read CA cert")
	ErrCACertParse = errors.New("failed to parse CA cert")
)

// ErrClusterUnavailable is returned when the NATS connection is not currently connected.
var ErrClusterUnavailable = errors.New("cluster unavailable: NATS disconnected")

// SubjectEnsureDefaultVpc is the request/reply subject on which the daemon
// builds an account's default VPC and acknowledges. It lives here rather than
// beside either party so the gateway does not import the daemon, nor the
// reverse.
const SubjectEnsureDefaultVpc = "ec2.EnsureDefaultVpc"

// LoadCertPool reads a PEM CA certificate from path and returns a pool
// trusting exactly it. Shared by every caller that verifies a peer against
// the cluster CA (e.g. /etc/spinifex/ca.pem) rather than the system trust
// store.
func LoadCertPool(path string) (*x509.CertPool, error) {
	caCert, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w %s: %w", ErrCACertRead, path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("%w from %s", ErrCACertParse, path)
	}
	return pool, nil
}

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

	reconnectWait := cfg.reconnectWait
	if reconnectWait <= 0 {
		reconnectWait = time.Second
	}

	natsOpts := []nats.Option{
		nats.ReconnectWait(reconnectWait),
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
		pool, err := LoadCertPool(caCertPath)
		if err != nil {
			return nil, err
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
	reconnectWait time.Duration
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

// WithReconnectWait overrides how long the client waits between reconnect
// attempts to the same server (default 1s).
func WithReconnectWait(d time.Duration) RetryOption {
	return func(c *retryConfig) { c.reconnectWait = d }
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
				slog.Info("NATS connection established", "elapsed_ms", otelsetup.Millis(time.Since(start)))
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
				slog.Error("NATS still disconnected", "error", err, "disconnected_for_ms", otelsetup.Millis(elapsed), "attempt", attempt)
				lastEscalatedLog = time.Now()
			}
		} else {
			slog.Warn("NATS not ready, retrying...", "error", err, "elapsed_ms", otelsetup.Millis(elapsed), "retry_in_ms", otelsetup.Millis(cfg.retryDelay), "attempt", attempt)
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

// decodedNATSError retains a response's diagnostic message while exposing its
// sanitized AWS wire code to unwrap-aware classifiers.
type decodedNATSError struct {
	code    error
	message string
}

func (e *decodedNATSError) Error() string {
	return e.message
}

func (e *decodedNATSError) Unwrap() error {
	return e.code
}

// NATSRequest performs a NATS request-response with JSON marshaling. It sends
// with X-Account-ID (plus any extra headers), unmarshals the successful response
// into Out, and carries ctx's trace context onto the wire: it opens a client span
// for the hop and injects traceparent so the consumer joins the same trace.
func NATSRequest[Out any](ctx context.Context, conn *nats.Conn, subject string, input any, timeout time.Duration, accountID string, headers ...NATSHeader) (out *Out, err error) {
	if conn == nil || !conn.IsConnected() {
		return nil, ErrClusterUnavailable
	}

	ctx, span := startProducerSpan(ctx, subject, accountID)
	defer func() { endSpanWithError(span, err) }()

	jsonData, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal input: %w", err)
	}

	reqMsg := nats.NewMsg(subject)
	reqMsg.Data = jsonData
	reqMsg.Header.Set(AccountIDHeader, accountID)
	// Forwarded here so every NATS-backed service inherits the caller's retry
	// token without widening its signature.
	if key := IdempotencyKeyFromContext(ctx); key != "" {
		reqMsg.Header.Set(IdempotencyKeyHeader, key)
	}
	InjectTraceContext(ctx, reqMsg.Header)
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
		if responseError.Message != nil && *responseError.Message != "" {
			return nil, &decodedNATSError{code: errors.New(*responseError.Code), message: *responseError.Message}
		}
		return nil, errors.New(*responseError.Code)
	}

	var output Out
	if err := json.Unmarshal(msg.Data, &output); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return &output, nil
}

// ServeNATSRequest unmarshals the request into *I, invokes fn, and replies with JSON or an awserrors envelope.
// A consumer span joins the producer's trace when the message carries traceparent.
// Reports whether the reply was a result rather than an error envelope, for
// callers that record a request outcome; callers that do not may ignore it.
func ServeNATSRequest[I any, O any](msg *nats.Msg, fn func(*I) (*O, error)) bool {
	return ServeNATSRequestCtx(msg, func(_ context.Context, in *I) (*O, error) { return fn(in) })
}

// ServeNATSRequestCtx is ServeNATSRequest for handlers that take the consumer
// span's context, so their logs and child spans correlate to the trace.
func ServeNATSRequestCtx[I any, O any](msg *nats.Msg, fn func(context.Context, *I) (*O, error)) bool {
	ctx, span := StartConsumerSpan(msg)
	defer span.End()

	input := new(I)
	if errResp := UnmarshalJsonPayload(input, msg.Data); errResp != nil {
		MarkSpanError(span, errors.New(awserrors.ErrorInvalidParameterValue))
		respondNATS(msg, errResp)
		return false
	}
	out, err := fn(ctx, input)
	if err != nil {
		MarkSpanError(span, err)
		respondNATS(msg, GenerateErrorPayloadWithMessage(awserrors.ValidErrorCodeFromError(err), err.Error()))
		return false
	}
	data, err := json.Marshal(out)
	if err != nil {
		MarkSpanError(span, err)
		respondNATS(msg, GenerateErrorPayload(awserrors.ErrorServerInternal))
		return false
	}
	respondNATS(msg, data)
	return true
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
	// maxIdentityGatherFrames caps total frames an identity-mode fan-out
	// (CollectUntilDeadline, or CollectServeData with ExpectedResponders set)
	// will process, independent of cluster size, so a stray publisher or a
	// hot restart loop cannot hold collection open for the full deadline.
	maxIdentityGatherFrames = 4096
	// maxIdentityGatherBytes caps total retained payload bytes for an
	// identity-mode fan-out. Only the first payload per node is retained, so
	// this bounds memory to roughly the cluster's healthy-case response size.
	maxIdentityGatherBytes = 64 * 1024 * 1024
)

// NodeIDHeader is the NATS reply header key carrying the responding daemon
// node's ID, set on every daemon reply so a fan-out can attribute a frame.
const NodeIDHeader = "X-Node-ID"

// NodeIDFromMsg extracts the responding node's ID from a NATS reply header, or "" if absent.
func NodeIDFromMsg(msg *nats.Msg) string {
	if msg == nil || msg.Header == nil {
		return ""
	}
	return msg.Header.Get(NodeIDHeader)
}

// CollectionMode governs when a Gather fan-out stops waiting for replies.
type CollectionMode int

const (
	// CollectServeData exits once its stop condition is met — ExpectedNodes
	// frames, ExpectedResponders unique nodes, or StopOnFirst. The zero value,
	// so every caller that does not opt in keeps today's exact behavior.
	CollectServeData CollectionMode = iota
	// CollectUntilDeadline collects for the full Timeout. A fan-out proving a
	// negative (nobody has it) cannot be established from a prefix of the
	// replies — the frame that would refute it is the one most likely to be
	// late. GatherOpts.Settled is the one way out, for the case where the
	// replies so far mean nothing is left to prove.
	CollectUntilDeadline
)

// Frame is one daemon reply, tagged with the responding node's ID ("" when
// the reply carried no node ID header, e.g. an older daemon).
type Frame struct {
	NodeID string
	Data   []byte
}

// Summary is a local tally of a fan-out; it is never sent over the wire.
type Summary struct {
	Received       int            // frames seen (success + error)
	Successes      int            // frames that decoded as a non-error envelope
	ErrorCodes     map[string]int // AWS error code -> count across error frames
	FirstClient4xx string         // first deterministic 4xx code seen, "" if none
	TimedOut       bool           // deadline hit before the stop condition was met

	// The rest are populated only in identity mode (ExpectedResponders set,
	// or Mode == CollectUntilDeadline). They stay nil/zero for a caller that
	// does not opt in, so that caller's behavior is unchanged by construction.
	Responders        map[string]bool // node ID -> answered at all (success or error); terminates collection under CollectServeData
	SuccessResponders map[string]bool // node ID -> answered with a decodable non-error frame
	ErrorResponders   map[string]bool // node ID -> answered with an error envelope
	ConflictNodes     map[string]bool // node ID -> replied more than once with payloads that disagree
	Unidentified      int             // frames received with no node ID header
	DuplicateFrames   int             // frames whose node had already answered; bytes dropped, identity kept
	CapHit            bool            // the identity frame or byte cap ended collection early
	SettledEarly      bool            // GatherOpts.Settled ended collection before the deadline
}

// GatherOpts configures a Gather fan-out.
type GatherOpts struct {
	Timeout            time.Duration  // hard deadline for the whole fan-out
	ExpectedNodes      int            // early-exit once this many frames arrive (0 = wait full Timeout); mutually exclusive with ExpectedResponders
	ExpectedResponders int            // identity mode: early-exit once this many distinct nodes have answered, under CollectServeData; mutually exclusive with ExpectedNodes
	Mode               CollectionMode // CollectServeData (default) or CollectUntilDeadline
	StopOnFirst        bool           // return after the first non-error frame (first-wins)
	AccountID          string         // sets X-Account-ID header when non-empty

	// Settled ends a CollectUntilDeadline fan-out early; it is ignored in every
	// other mode. Consulted after each retained frame, it returns true once the
	// replies already answer the question, so only a caller still trying to
	// prove a negative pays the rest of the deadline.
	Settled func(frames []Frame, sum Summary) bool
}

// Gather publishes payload to subject over a fresh inbox and collects reply
// frames until the stop condition for opts.Mode is met, or Timeout elapses.
// Error envelopes and oversized frames are dropped from frames but counted in
// sum; returned frames are raw daemon replies for the caller to decode and
// merge, tagged with the responding node's ID when the reply carried one.
//
// With ExpectedResponders unset and Mode left at its zero value
// (CollectServeData), Gather's behavior is unchanged from before identity
// mode existed: the stop condition is ExpectedNodes frames (or the unbounded
// cap), and Summary's identity fields stay nil/zero. Identity mode — set
// either by ExpectedResponders or by CollectUntilDeadline — additionally
// tracks per-node responder sets, retains only the first payload seen from
// each node, and is bounded by its own frame and byte caps independent of
// ExpectedNodes. It carries ctx's trace context onto the wire: it opens a
// producer span for the fan-out and injects traceparent so every consumer
// joins the same trace.
func Gather(ctx context.Context, conn *nats.Conn, subject string, payload []byte, opts GatherOpts) (frames []Frame, sum Summary, err error) {
	sum.ErrorCodes = map[string]int{}
	if conn == nil || !conn.IsConnected() {
		return nil, sum, ErrClusterUnavailable
	}
	if opts.ExpectedNodes > 0 && opts.ExpectedResponders > 0 {
		return nil, sum, fmt.Errorf("gather: ExpectedNodes and ExpectedResponders are mutually exclusive")
	}

	ctx, span := startProducerSpan(ctx, subject, opts.AccountID)
	defer func() { endSpanWithError(span, err) }()

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
	InjectTraceContext(ctx, pubMsg.Header)
	if err := conn.PublishMsg(pubMsg); err != nil {
		return nil, sum, fmt.Errorf("failed to publish request: %w", err)
	}

	identityMode := opts.ExpectedResponders > 0 || opts.Mode == CollectUntilDeadline
	earlyExitOnResponders := opts.Mode == CollectServeData && opts.ExpectedResponders > 0

	var seenHash map[string][32]byte
	if identityMode {
		sum.Responders = map[string]bool{}
		sum.SuccessResponders = map[string]bool{}
		sum.ErrorResponders = map[string]bool{}
		sum.ConflictNodes = map[string]bool{}
		seenHash = map[string][32]byte{}
	}

	maxResponses := maxScatterGatherUnboundedResponses
	if opts.ExpectedNodes > 0 {
		maxResponses = opts.ExpectedNodes
	}

	var retainedBytes int
	deadline := time.Now().Add(opts.Timeout)
	for {
		if earlyExitOnResponders {
			if len(sum.Responders) >= opts.ExpectedResponders {
				break
			}
		} else if !identityMode && sum.Received >= maxResponses {
			break
		}

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

		nodeID := NodeIDFromMsg(msg)

		if identityMode {
			if sum.Received > maxIdentityGatherFrames || retainedBytes > maxIdentityGatherBytes {
				sum.CapHit = true
				break
			}

			if nodeID == "" {
				sum.Unidentified++
			} else if prevHash, seen := seenHash[nodeID]; seen {
				// A later frame from a node already seen: keep the identity and
				// flag disagreement, but never let its bytes overwrite the first
				// payload — there is no merge rule for two contradictory answers
				// from one node, and inventing one would hide the fault.
				sum.DuplicateFrames++
				if sha256.Sum256(msg.Data) != prevHash {
					sum.ConflictNodes[nodeID] = true
				}
				if _, verr := ValidateErrorPayload(msg.Data); verr != nil {
					sum.ErrorResponders[nodeID] = true
				} else {
					sum.SuccessResponders[nodeID] = true
				}
				continue
			}
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
			if identityMode && nodeID != "" {
				sum.Responders[nodeID] = true
				sum.ErrorResponders[nodeID] = true
				seenHash[nodeID] = sha256.Sum256(msg.Data)
			}
			continue
		}

		sum.Successes++
		if identityMode && nodeID != "" {
			sum.Responders[nodeID] = true
			sum.SuccessResponders[nodeID] = true
			seenHash[nodeID] = sha256.Sum256(msg.Data)
			retainedBytes += len(msg.Data)
		}
		frames = append(frames, Frame{NodeID: nodeID, Data: msg.Data})
		if opts.StopOnFirst {
			return frames, sum, nil
		}
		if opts.Mode == CollectUntilDeadline && opts.Settled != nil && opts.Settled(frames, sum) {
			sum.SettledEarly = true
			break
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

// addNATTimeout bounds the vpc.add-nat request-reply. vpcd's handler holds the
// reply until the flows barrier (ovn-nbctl --wait=hv sync, bounded 30 s) confirms
// every chassis realised the rule. On a cold multi-node cluster the gateway SB
// chassisredirect binding can take most of that budget to converge, so the caller
// deadline must exceed 30 s or it abandons a still-committing rule and rolls back
// the EIP mid-launch — surfacing as a spurious ServerInternal on RunInstances.
const addNATTimeout = 45 * time.Second

// AddNAT requests vpcd commit the OVN dnat_and_snat rule via NATS request-reply.
// A non-nil return means the rule may not be committed; callers must roll back and publish vpc.delete-nat.
func AddNAT(nc *nats.Conn, vpcID, externalIP, logicalIP, portName, mac string) error {
	return RequestEvent(nc, "vpc.add-nat", natEvent{
		VpcId: vpcID, ExternalIP: externalIP, LogicalIP: logicalIP,
		PortName: portName, MAC: mac,
	}, addNATTimeout)
}

// PublishNATEvent sends a NAT lifecycle event. vpc.add-nat uses request-reply (prevents ARP races);
// vpc.delete-nat is fire-and-forget. Use AddNAT directly when failure must trigger a rollback.
func PublishNATEvent(nc *nats.Conn, topic, vpcID, externalIP, logicalIP, portName, mac string) {
	evt := natEvent{
		VpcId: vpcID, ExternalIP: externalIP, LogicalIP: logicalIP,
		PortName: portName, MAC: mac,
	}

	if topic == "vpc.add-nat" {
		if err := RequestEvent(nc, topic, evt, addNATTimeout); err != nil {
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
