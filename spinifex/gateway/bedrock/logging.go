package gateway_bedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrock"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/nats-io/nats.go/jetstream"
)

// Bedrock-runtime operation names carried on every InvocationRecord.
const (
	OperationConverse                      = "Converse"
	OperationConverseStream                = "ConverseStream"
	OperationInvokeModel                   = "InvokeModel"
	OperationInvokeModelWithResponseStream = "InvokeModelWithResponseStream"
)

const (
	// InvocationStreamName is the JetStream stream carrying every Bedrock
	// invocation record. LimitsPolicy retention (not WorkQueuePolicy) is
	// required: two independent durable consumers attach to it — this
	// package's own S3+ES delivery consumer (InvocationDeliveryConsumer) and
	// a separate usage/cost-aggregation consumer owned elsewhere — and each
	// must see every message regardless of the other's ack position.
	InvocationStreamName = "OCHRE_INVOCATIONS"

	// InvocationStreamSubject is the single subject the stream captures.
	InvocationStreamSubject = "bedrock.invocations"

	// InvocationDeliveryConsumer is this package's durable pull consumer:
	// one record in, one S3 write plus one metadata-only log line out.
	InvocationDeliveryConsumer = "ochre-invocation-delivery"

	// invocationStreamMaxAge bounds disk growth for a stream nothing besides
	// its durable consumers is required to keep forever.
	invocationStreamMaxAge = 30 * 24 * time.Hour
)

// EnsureInvocationStream idempotently creates (or updates) the invocation
// stream. Safe to call from every gateway node at boot. replicas must match
// the cluster's node count: at replicas=1, losing the one node holding this
// stream loses billing/audit records even though the control plane survives
// on the others, defeating the point of using JetStream over a lossy buffer.
func EnsureInvocationStream(ctx context.Context, js jetstream.JetStream, replicas int) (jetstream.Stream, error) {
	stream, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      InvocationStreamName,
		Subjects:  []string{InvocationStreamSubject},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.FileStorage,
		MaxAge:    invocationStreamMaxAge,
		Replicas:  replicas,
	})
	if err != nil {
		return nil, fmt.Errorf("ensure invocation stream: %w", err)
	}
	return stream, nil
}

// EnsureDeliveryConsumer idempotently creates (or updates) this package's
// durable pull consumer on the invocation stream.
func EnsureDeliveryConsumer(ctx context.Context, js jetstream.JetStream) (jetstream.Consumer, error) {
	consumer, err := js.CreateOrUpdateConsumer(ctx, InvocationStreamName, jetstream.ConsumerConfig{
		Durable:       InvocationDeliveryConsumer,
		FilterSubject: InvocationStreamSubject,
		AckPolicy:     jetstream.AckExplicitPolicy,
		MaxDeliver:    5,
		AckWait:       time.Minute,
	})
	if err != nil {
		return nil, fmt.Errorf("ensure invocation delivery consumer: %w", err)
	}
	return consumer, nil
}

// errCodeClientDisconnected marks an invocation record ended because the
// client went away (context cancelled, or a frame write failed), not because
// of any upstream/model fault. It is this package's own bookkeeping value,
// not a registered AWS error code — InvocationRecord is an internal audit
// trail, not an AWS API response.
const errCodeClientDisconnected = "ClientDisconnected"

// InvocationRecord is one Bedrock invocation's durable audit trail: what was
// called, by whom, at what cost, and how it ended. It is published to the
// invocation stream on every exit path (clean end, client disconnect,
// upstream fault) and fanned out from there to per-account S3 delivery and
// the platform's metadata-only usage/cost sink.
//
// InputText/OutputText are populated by the caller but are only actually
// delivered when StreamRecorder finds AccountID's logging config enables
// text delivery; every other field is metadata safe for the shared platform
// log sink to read.
type InvocationRecord struct {
	RequestID      string    `json:"requestId"`
	AccountID      string    `json:"accountId"`
	ModelID        string    `json:"modelId"`
	Operation      string    `json:"operation"`
	Backend        string    `json:"backend"`
	Timestamp      time.Time `json:"timestamp"`
	LatencyMs      int64     `json:"latencyMs"`
	HTTPStatus     int       `json:"httpStatus"`
	ErrorCode      string    `json:"errorCode,omitempty"`
	InputTokens    int64     `json:"inputTokens"`
	OutputTokens   int64     `json:"outputTokens"`
	UsageEstimated bool      `json:"usageEstimated"`
	// Partial marks a record whose invocation did not reach a clean end
	// (client disconnect, upstream fault, mid-stream cancellation). Partial
	// records are still billable — usage reflects whatever tokens were
	// actually generated — but flagged so downstream aggregation can treat
	// them distinctly if it chooses to.
	Partial    bool   `json:"partial"`
	InputText  string `json:"inputText,omitempty"`
	OutputText string `json:"outputText,omitempty"`
}

// Recorder durably records one invocation. Implementations MUST NOT let a
// recording failure fail the invocation itself — publish errors are logged
// and counted, never returned, so a struggling stream never takes billable
// model traffic down with it.
type Recorder interface {
	Record(ctx context.Context, rec InvocationRecord)
}

// usageReporter is an optional capability a streaming source may implement
// to expose token usage collected so far, for the invocation record the
// pump emits on every exit path. vLLM only learns real usage from its
// trailing usage chunk, so it falls back to a content-delta-chunk output
// estimate (estimated=true) when the stream ends before that arrives.
// Anthropic streams real incremental usage throughout and never estimates —
// unlike self-host generation, its tokens carry real external cost.
type usageReporter interface {
	usage() (inputTokens, outputTokens int64, estimated bool)
}

// streamRecordCtx carries the fixed per-call metadata a streaming pump needs
// to build its InvocationRecord once the stream ends, on every exit path.
type streamRecordCtx struct {
	recorder  Recorder
	requestID string
	accountID string
	backend   string
	operation string
	start     time.Time
	inputText string
}

type noopRecorder struct{}

func (noopRecorder) Record(context.Context, InvocationRecord) {}

// NoopRecorder discards every record. Used as the fallback wherever no
// StreamRecorder is configured (e.g. unit tests of unrelated routes).
var NoopRecorder Recorder = noopRecorder{}

var _ Recorder = noopRecorder{}

// recordOutcome maps a Converse/InvokeModel error to the HTTP status and
// AWS error code an InvocationRecord carries. A nil err is success.
func recordOutcome(err error) (httpStatus int, code string) {
	if err == nil {
		return http.StatusOK, ""
	}
	code = awserrors.ValidErrorCodeFromError(err)
	return awserrors.LookupErrorMessage("bedrock-runtime", code).HTTPCode, code
}

// extractTokenUsage best-effort parses a non-streaming InvokeModel response
// body for real token counts, reusing each backend's own response struct
// rather than a bespoke shape. ok is false when the body doesn't decode —
// callers must treat that as "unknown", not as a zero-token estimate.
func extractTokenUsage(backend string, respBody []byte) (inputTokens, outputTokens int64, ok bool) {
	switch {
	case backend == tierSelfHost:
		var lr llamaInvokeResponse
		if json.Unmarshal(respBody, &lr) != nil {
			return 0, 0, false
		}
		return int64(lr.PromptTokenCount), int64(lr.GenerationTokenCount), true
	case strings.HasPrefix(backend, providerPrefix):
		var ar anthropicResponse
		if json.Unmarshal(respBody, &ar) != nil {
			return 0, 0, false
		}
		return ar.Usage.InputTokens, ar.Usage.OutputTokens, true
	default:
		return 0, 0, false
	}
}

// recordPublishTimeout bounds how long Record waits for the JetStream
// publish once detached from the caller's (possibly already-cancelled)
// context, so a struggling stream can't hang a pump's defer indefinitely.
const recordPublishTimeout = 5 * time.Second

// StreamRecorder publishes InvocationRecords to the invocation stream,
// including body text only when the account's own logging config enables
// delivery — LoggingConfigReader is consulted per record, never cached past
// a single Record call, so a config change takes effect immediately.
//
// A publish failure never fails the caller's invocation: it is logged and
// counted (Dropped), matching the platform's stance that billing/audit
// durability must never come at the cost of the model call itself.
type StreamRecorder struct {
	js      jetstream.JetStream
	configs LoggingConfigReader

	dropped atomic.Int64
}

var _ Recorder = (*StreamRecorder)(nil)

// NewStreamRecorder constructs a StreamRecorder. A nil configs falls back to
// a reader that never enables body delivery, so records always carry
// metadata only until a real LoggingConfigStore is wired in.
func NewStreamRecorder(js jetstream.JetStream, configs LoggingConfigReader) *StreamRecorder {
	if configs == nil {
		configs = noopLoggingConfigReader{}
	}
	return &StreamRecorder{js: js, configs: configs}
}

// Dropped returns the number of records this recorder has failed to
// publish, for monitoring and tests.
func (r *StreamRecorder) Dropped() int64 {
	return r.dropped.Load()
}

// Record fills in Timestamp, clears body text unless AccountID's logging
// config enables it, and publishes to the invocation stream keyed by
// RequestID (jetstream.WithMsgID) so a retried publish dedupes downstream.
// ctx is detached from the caller's cancellation — a client disconnect must
// not prevent recording the very disconnect it caused — but bounded by
// recordPublishTimeout so a stalled stream can't hang the caller's defer.
func (r *StreamRecorder) Record(ctx context.Context, rec InvocationRecord) {
	rec.Timestamp = time.Now()

	cfg, enabled, err := r.configs.Get(ctx, rec.AccountID)
	if err != nil {
		slog.Error("bedrock: logging config lookup failed, dropping record body", "account", rec.AccountID, "err", err)
		enabled = false
	}
	if !enabled || !cfg.TextDataDeliveryEnabled {
		rec.InputText = ""
		rec.OutputText = ""
	}

	payload, err := json.Marshal(rec)
	if err != nil {
		slog.Error("bedrock: failed to marshal invocation record", "request_id", rec.RequestID, "err", err)
		r.dropped.Add(1)
		return
	}

	pubCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordPublishTimeout)
	defer cancel()
	if _, err := r.js.Publish(pubCtx, InvocationStreamSubject, payload, jetstream.WithMsgID(rec.RequestID)); err != nil {
		slog.Error("bedrock: failed to publish invocation record", "request_id", rec.RequestID, "account", rec.AccountID, "err", err)
		r.dropped.Add(1)
	}
}

// LoggingConfig is the account's own invocation-logging preferences: where
// to deliver, and whether body text may be included at all.
type LoggingConfig struct {
	S3BucketName             string `json:"s3BucketName"`
	S3KeyPrefix              string `json:"s3KeyPrefix"`
	TextDataDeliveryEnabled  bool   `json:"textDataDeliveryEnabled"`
	ImageDataDeliveryEnabled bool   `json:"imageDataDeliveryEnabled"`
}

// LoggingConfigReader resolves accountID's stored logging config. ok is
// false when the account has never configured one (delivery disabled).
type LoggingConfigReader interface {
	Get(ctx context.Context, accountID string) (LoggingConfig, bool, error)
}

type noopLoggingConfigReader struct{}

func (noopLoggingConfigReader) Get(context.Context, string) (LoggingConfig, bool, error) {
	return LoggingConfig{}, false, nil
}

// bedrockLoggingConfigBucket is the cluster-replicated KV bucket holding
// per-account invocation-logging configuration.
const bedrockLoggingConfigBucket = "bedrock-logging-config"

const bedrockLoggingConfigHistory = 1

// LoggingConfigStore persists per-account LoggingConfig in the
// bedrock-logging-config JetStream KV bucket, and serves as the
// LoggingConfigReader StreamRecorder consults before including body text.
type LoggingConfigStore struct {
	js       jetstream.JetStream
	replicas int

	mu sync.Mutex
	kv jetstream.KeyValue
}

var _ LoggingConfigReader = (*LoggingConfigStore)(nil)

// NewLoggingConfigStore constructs a LoggingConfigStore.
func NewLoggingConfigStore(js jetstream.JetStream, replicas int) *LoggingConfigStore {
	return &LoggingConfigStore{js: js, replicas: replicas}
}

// bucket lazily opens (or creates) the cluster-replicated
// bedrock-logging-config KV bucket, caching the handle for subsequent calls.
func (s *LoggingConfigStore) bucket(ctx context.Context) (jetstream.KeyValue, error) {
	if s.js == nil {
		return nil, errors.New("bedrock: logging config store has no JetStream client configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kv != nil {
		return s.kv, nil
	}
	kv, err := kvutil.GetOrCreateBucketWithReplicas(ctx, s.js, bedrockLoggingConfigBucket, bedrockLoggingConfigHistory, s.replicas)
	if err != nil {
		return nil, err
	}
	s.kv = kv
	return kv, nil
}

// Get returns accountID's stored logging config, or (zero, false, nil) if
// the account has never configured one.
func (s *LoggingConfigStore) Get(ctx context.Context, accountID string) (LoggingConfig, bool, error) {
	kv, err := s.bucket(ctx)
	if err != nil {
		return LoggingConfig{}, false, err
	}
	entry, err := kv.Get(ctx, accountID)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return LoggingConfig{}, false, nil
		}
		return LoggingConfig{}, false, fmt.Errorf("kv get logging config for %s: %w", accountID, err)
	}
	var cfg LoggingConfig
	if err := json.Unmarshal(entry.Value(), &cfg); err != nil {
		return LoggingConfig{}, false, fmt.Errorf("decode logging config for %s: %w", accountID, err)
	}
	return cfg, true, nil
}

// Put stores accountID's logging config, overwriting any existing one.
func (s *LoggingConfigStore) Put(ctx context.Context, accountID string, cfg LoggingConfig) error {
	kv, err := s.bucket(ctx)
	if err != nil {
		return err
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode logging config for %s: %w", accountID, err)
	}
	if _, err := kv.Put(ctx, accountID, data); err != nil {
		return fmt.Errorf("kv put logging config for %s: %w", accountID, err)
	}
	return nil
}

// Delete removes accountID's logging config, treating an already-absent
// config as success.
func (s *LoggingConfigStore) Delete(ctx context.Context, accountID string) error {
	kv, err := s.bucket(ctx)
	if err != nil {
		return err
	}
	if err := kv.Delete(ctx, accountID); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("kv delete logging config for %s: %w", accountID, err)
	}
	return nil
}

// loggingConfigFromSDK validates and translates a *bedrock.LoggingConfig
// into the package's internal LoggingConfig. CloudWatchConfig is rejected:
// this platform's only delivery target is the account's own S3 bucket —
// body text is never routed to a platform-shared sink.
func loggingConfigFromSDK(in *bedrock.LoggingConfig) (LoggingConfig, error) {
	if in == nil {
		return LoggingConfig{}, errors.New(awserrors.ErrorValidationException)
	}
	if in.CloudWatchConfig != nil {
		return LoggingConfig{}, errors.New(awserrors.ErrorValidationException)
	}
	textEnabled := aws.BoolValue(in.TextDataDeliveryEnabled)
	imageEnabled := aws.BoolValue(in.ImageDataDeliveryEnabled)
	if (textEnabled || imageEnabled) && (in.S3Config == nil || aws.StringValue(in.S3Config.BucketName) == "") {
		return LoggingConfig{}, errors.New(awserrors.ErrorValidationException)
	}
	cfg := LoggingConfig{
		TextDataDeliveryEnabled:  textEnabled,
		ImageDataDeliveryEnabled: imageEnabled,
	}
	if in.S3Config != nil {
		cfg.S3BucketName = aws.StringValue(in.S3Config.BucketName)
		cfg.S3KeyPrefix = aws.StringValue(in.S3Config.KeyPrefix)
	}
	return cfg, nil
}

// loggingConfigToSDK translates the package's internal LoggingConfig back to
// the AWS-parity *bedrock.LoggingConfig shape.
func loggingConfigToSDK(cfg LoggingConfig) *bedrock.LoggingConfig {
	out := &bedrock.LoggingConfig{
		TextDataDeliveryEnabled:  aws.Bool(cfg.TextDataDeliveryEnabled),
		ImageDataDeliveryEnabled: aws.Bool(cfg.ImageDataDeliveryEnabled),
	}
	if cfg.S3BucketName != "" {
		out.S3Config = &bedrock.S3Config{
			BucketName: aws.String(cfg.S3BucketName),
			KeyPrefix:  aws.String(cfg.S3KeyPrefix),
		}
	}
	return out
}

// PutModelInvocationLoggingConfiguration stores accountID's invocation
// logging preferences. CloudWatch delivery is not supported (body text is
// delivered only to the account's own S3 bucket, never a platform-shared
// sink); a request naming it is rejected.
func PutModelInvocationLoggingConfiguration(ctx context.Context, accountID string, store *LoggingConfigStore, input *bedrock.PutModelInvocationLoggingConfigurationInput) (*bedrock.PutModelInvocationLoggingConfigurationOutput, error) {
	cfg, err := loggingConfigFromSDK(input.LoggingConfig)
	if err != nil {
		return nil, err
	}
	if err := store.Put(ctx, accountID, cfg); err != nil {
		return nil, err
	}
	return &bedrock.PutModelInvocationLoggingConfigurationOutput{}, nil
}

// GetModelInvocationLoggingConfiguration returns accountID's stored logging
// config, or an empty LoggingConfig when none has been set.
func GetModelInvocationLoggingConfiguration(ctx context.Context, accountID string, store *LoggingConfigStore, _ *bedrock.GetModelInvocationLoggingConfigurationInput) (*bedrock.GetModelInvocationLoggingConfigurationOutput, error) {
	cfg, ok, err := store.Get(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return &bedrock.GetModelInvocationLoggingConfigurationOutput{}, nil
	}
	return &bedrock.GetModelInvocationLoggingConfigurationOutput{LoggingConfig: loggingConfigToSDK(cfg)}, nil
}

// DeleteModelInvocationLoggingConfiguration removes accountID's stored
// logging config.
func DeleteModelInvocationLoggingConfiguration(ctx context.Context, accountID string, store *LoggingConfigStore, _ *bedrock.DeleteModelInvocationLoggingConfigurationInput) (*bedrock.DeleteModelInvocationLoggingConfigurationOutput, error) {
	if err := store.Delete(ctx, accountID); err != nil {
		return nil, err
	}
	return &bedrock.DeleteModelInvocationLoggingConfigurationOutput{}, nil
}

// DeliveryConsumer drains InvocationDeliveryConsumer, writing each record's
// body (if present) to the account's own S3 bucket and always emitting one
// structured slog line carrying metadata only — the platform's log-shipping
// pipeline (filebeat/Alloy -> Elasticsearch) is the "ES delivery" path,
// there being no bespoke Elasticsearch client in this codebase. The slog
// call reads named metadata fields only and never references
// rec.InputText/rec.OutputText, so the no-body-reaches-ES invariant holds
// structurally even if StreamRecorder's own gating were ever bypassed.
type DeliveryConsumer struct {
	store   objectstore.ObjectStore
	configs LoggingConfigReader
}

// NewDeliveryConsumer constructs a DeliveryConsumer. store is the
// platform-trusted object store client (the same one the ECR registry
// writes through) — the account's own bucket name comes from its logging
// config, not a per-account credential, mirroring how Bedrock's own service
// role writes into a customer bucket, simplified for this platform's trust
// model.
func NewDeliveryConsumer(store objectstore.ObjectStore, configs LoggingConfigReader) *DeliveryConsumer {
	return &DeliveryConsumer{store: store, configs: configs}
}

// Run pulls records from consumer until ctx is cancelled, acking each after
// successful delivery and nak'ing on failure so JetStream redelivers it.
func (c *DeliveryConsumer) Run(ctx context.Context, consumer jetstream.Consumer) {
	iter, err := consumer.Messages()
	if err != nil {
		slog.Error("bedrock: failed to start invocation delivery consumer", "err", err)
		return
	}
	defer iter.Stop()

	// iter.Next() blocks indefinitely when no message is pending, so ctx
	// cancellation alone would never unblock the loop below — Stop() is what
	// actually wakes it, letting Next() return ErrMsgIteratorClosed.
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			iter.Stop()
		case <-stopped:
		}
	}()

	for {
		msg, err := iter.Next()
		if err != nil {
			if errors.Is(err, jetstream.ErrMsgIteratorClosed) || ctx.Err() != nil {
				return
			}
			slog.Error("bedrock: invocation delivery consumer fetch failed", "err", err)
			continue
		}
		c.deliver(ctx, msg)
	}
}

// deliver decodes, writes, and acks (or naks) one message. A decode failure
// acks and drops the message outright: it can never succeed on redelivery.
func (c *DeliveryConsumer) deliver(ctx context.Context, msg jetstream.Msg) {
	var rec InvocationRecord
	if err := json.Unmarshal(msg.Data(), &rec); err != nil {
		slog.Error("bedrock: invocation delivery consumer: undecodable record, dropping", "err", err)
		if ackErr := msg.Ack(); ackErr != nil {
			slog.Error("bedrock: invocation delivery consumer: ack failed", "err", ackErr)
		}
		return
	}

	// deliverS3 is unconditional: any account with a resolvable S3 bucket
	// gets its invocation records, body or not. The delivery *flags*
	// (TextDataDeliveryEnabled et al) already gated InputText/OutputText
	// back in Record — this is not a second gate on top of that one.
	if err := c.deliverS3(ctx, rec); err != nil {
		slog.Error("bedrock: invocation delivery consumer: S3 delivery failed", "request_id", rec.RequestID, "account", rec.AccountID, "err", err)
		if nakErr := msg.Nak(); nakErr != nil {
			slog.Error("bedrock: invocation delivery consumer: nak failed", "err", nakErr)
		}
		return
	}

	// Metadata only: never rec.InputText/rec.OutputText.
	slog.Info("bedrock invocation",
		"request_id", rec.RequestID,
		"account_id", rec.AccountID,
		"model_id", rec.ModelID,
		"operation", rec.Operation,
		"backend", rec.Backend,
		"timestamp", rec.Timestamp,
		"latency_ms", rec.LatencyMs,
		"http_status", rec.HTTPStatus,
		"error_code", rec.ErrorCode,
		"input_tokens", rec.InputTokens,
		"output_tokens", rec.OutputTokens,
		"usage_estimated", rec.UsageEstimated,
		"partial", rec.Partial,
	)

	if err := msg.Ack(); err != nil {
		slog.Error("bedrock: invocation delivery consumer: ack failed", "request_id", rec.RequestID, "err", err)
	}
}

// deliverS3 writes rec's body to the account's own bucket, or does nothing
// if the account has since disabled/removed delivery.
func (c *DeliveryConsumer) deliverS3(ctx context.Context, rec InvocationRecord) error {
	cfg, ok, err := c.configs.Get(ctx, rec.AccountID)
	if err != nil {
		return fmt.Errorf("resolve logging config: %w", err)
	}
	if !ok || cfg.S3BucketName == "" {
		return nil
	}

	if err := c.store.EnsureBucket(ctx, cfg.S3BucketName); err != nil {
		return fmt.Errorf("ensure bucket %s: %w", cfg.S3BucketName, err)
	}

	body, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal record: %w", err)
	}

	key := s3ObjectKey(cfg.S3KeyPrefix, rec)
	if _, err := c.store.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(cfg.S3BucketName),
		Key:    aws.String(key),
		Body:   bytes.NewReader(body),
	}); err != nil {
		return fmt.Errorf("put object %s/%s: %w", cfg.S3BucketName, key, err)
	}
	return nil
}

// s3ObjectKey lays records out as
// <prefix>/<modelId>/<year>/<month>/<day>/<requestId>.json, so an account's
// bucket stays browsable by model and date without a manifest.
func s3ObjectKey(prefix string, rec InvocationRecord) string {
	var parts []string
	if prefix != "" {
		parts = append(parts, strings.Trim(prefix, "/"))
	}
	parts = append(parts,
		rec.ModelID,
		rec.Timestamp.Format("2006/01/02"),
		rec.RequestID+".json",
	)
	return strings.Join(parts, "/")
}
