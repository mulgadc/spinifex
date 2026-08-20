package gateway_bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// bedrockUsageBucket is the cluster-replicated KV bucket holding per
	// account/model/period usage counters (D8, D12), keyed
	// "{accountID}/{modelID-base64url}/{period}".
	bedrockUsageBucket  = "bedrock-usage"
	bedrockUsageHistory = 1

	// bedrockUsageDedupeBucket records every RequestID the usage consumer has
	// already applied. At-least-once JetStream delivery means the same
	// record can arrive more than once; kv.Create is atomic
	// create-if-absent, so it doubles as the dedupe gate with no CAS loop of
	// its own required. The TTL matches the invocation stream's own
	// retention (invocationStreamMaxAge): once a record could no longer be
	// redelivered, its dedupe marker has nothing left to guard against.
	bedrockUsageDedupeBucket  = "bedrock-usage-dedupe"
	bedrockUsageDedupeHistory = 1

	// usageCASRetries bounds Apply's retry on a revision conflict, mirroring
	// handlers_quota's vcpuCASRetries.
	usageCASRetries = 100

	// UsageConsumerName is this package's durable pull consumer for usage and
	// cost aggregation, distinct from InvocationDeliveryConsumer: both attach
	// to the same OCHRE_INVOCATIONS stream (LimitsPolicy retention lets both
	// see every message independent of the other's ack position) but own
	// separate ack state, so redelivery on one never double-processes on the
	// other.
	UsageConsumerName = "ochre-usage-metering"
)

// billingPeriod returns the monthly billing period key for t, e.g. "2026-08".
// Monthly is the only period this package understands; the tokens-per-month
// quota dimension reads the same key shape.
func billingPeriod(t time.Time) string {
	return t.UTC().Format("2006-01")
}

// usageKey returns the KV key for accountID's modelID usage in period.
// modelID is base64url-encoded via weightsKey (defined in weights.go, same
// package) since model IDs contain ':', which NATS rejects in a KV key.
func usageKey(accountID, modelID, period string) string {
	return accountID + "/" + weightsKey(modelID) + "/" + period
}

// UsageCounters is one account's accrued Bedrock usage for one model in one
// billing period.
type UsageCounters struct {
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
	RequestCount int64 `json:"requestCount"`
	// CostMicroUSD is integer micro-USD (D12): floats drift across millions
	// of increments, and drift in a billing counter is a customer dispute
	// months later. Self-hosted contributions are always +0.
	CostMicroUSD int64 `json:"costMicroUsd"`
	// CostUnknown is sticky: once a contributing invocation had no
	// resolvable price, CostMicroUSD under-counts that invocation forever,
	// so the aggregate can never be called authoritative again for this
	// period even if a later invocation's price does resolve. It never
	// reverts to false once set, and must never be confused with a genuine
	// zero-cost period (self-host, or a provider period with no usage).
	CostUnknown bool `json:"costUnknown"`
}

// UsageReader resolves accountID's total token usage (input+output, across
// every model) for the current billing period. Satisfied by UsageStore; the
// quota package depends on this shape without importing this package (see
// handlers_quota.BedrockUsageReader).
type UsageReader interface {
	TokensThisPeriod(ctx context.Context, accountID string) (int64, error)
}

// UsageStore persists per-account/model/period UsageCounters in the
// bedrock-usage JetStream KV bucket, and dedupes applied RequestIDs in a
// second bucket so at-least-once stream delivery never double-counts.
type UsageStore struct {
	js       jetstream.JetStream
	replicas int

	mu sync.Mutex
	kv jetstream.KeyValue

	dedupeMu sync.Mutex
	dedupe   jetstream.KeyValue
}

var _ UsageReader = (*UsageStore)(nil)

// NewUsageStore constructs a UsageStore over the cluster's JetStream client,
// replicated across replicas nodes.
func NewUsageStore(js jetstream.JetStream, replicas int) *UsageStore {
	return &UsageStore{js: js, replicas: replicas}
}

// bucket lazily opens (or creates) the cluster-replicated bedrock-usage KV
// bucket, caching the handle for subsequent calls.
func (s *UsageStore) bucket(ctx context.Context) (jetstream.KeyValue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kv != nil {
		return s.kv, nil
	}
	kv, err := kvutil.GetOrCreateBucketWithReplicas(ctx, s.js, bedrockUsageBucket, bedrockUsageHistory, s.replicas)
	if err != nil {
		return nil, err
	}
	s.kv = kv
	return kv, nil
}

// dedupeBucket lazily opens (or creates) the cluster-replicated
// bedrock-usage-dedupe KV bucket.
func (s *UsageStore) dedupeBucket(ctx context.Context) (jetstream.KeyValue, error) {
	s.dedupeMu.Lock()
	defer s.dedupeMu.Unlock()
	if s.dedupe != nil {
		return s.dedupe, nil
	}
	kv, err := kvutil.GetOrCreateBucketWithTTL(ctx, s.js, bedrockUsageDedupeBucket, bedrockUsageDedupeHistory, invocationStreamMaxAge)
	if err != nil {
		return nil, err
	}
	s.dedupe = kv
	return kv, nil
}

// MarkProcessed atomically claims requestID for the usage consumer. first is
// true the one time this requestID is claimed; every subsequent call
// (redelivery) returns false with no error, so the caller can skip
// re-applying counters without treating the duplicate as a failure.
func (s *UsageStore) MarkProcessed(ctx context.Context, requestID string) (first bool, err error) {
	kv, err := s.dedupeBucket(ctx)
	if err != nil {
		return false, err
	}
	if _, err := kv.Create(ctx, requestID, []byte("1")); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return false, nil
		}
		return false, fmt.Errorf("claim dedupe marker for %s: %w", requestID, err)
	}
	return true, nil
}

// Get returns accountID's usage counters for modelID in period, or
// (zero, false, nil) if the account has accrued nothing yet.
func (s *UsageStore) Get(ctx context.Context, accountID, modelID, period string) (UsageCounters, bool, error) {
	kv, err := s.bucket(ctx)
	if err != nil {
		return UsageCounters{}, false, err
	}
	entry, err := kv.Get(ctx, usageKey(accountID, modelID, period))
	switch {
	case err == nil:
		var counters UsageCounters
		if uerr := json.Unmarshal(entry.Value(), &counters); uerr != nil {
			return UsageCounters{}, false, fmt.Errorf("decode usage counters for %s/%s/%s: %w", accountID, modelID, period, uerr)
		}
		return counters, true, nil
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return UsageCounters{}, false, nil
	default:
		return UsageCounters{}, false, fmt.Errorf("kv get usage counters for %s/%s/%s: %w", accountID, modelID, period, err)
	}
}

// microUSDCost returns tokens priced at pricePerMillion integer micro-USD
// per million tokens, truncated toward zero. Truncation is deliberate and
// consistent per record — it never accumulates in the customer's
// disfavour, and it keeps the arithmetic exact integer division with no
// float involved anywhere in the accrual path (D12).
func microUSDCost(tokens, pricePerMillion int64) int64 {
	return tokens * pricePerMillion / 1_000_000
}

// Apply adds rec's token counts and (if price is known) accrued cost to its
// account/model/period counter, under CAS so concurrent consumers never lose
// an update. Callers MUST have already confirmed via MarkProcessed that this
// RequestID has not been applied before — Apply itself has no idempotency of
// its own and will double-count a record applied twice.
func (s *UsageStore) Apply(ctx context.Context, rec InvocationRecord, price Price) error {
	kv, err := s.bucket(ctx)
	if err != nil {
		return err
	}
	key := usageKey(rec.AccountID, rec.ModelID, billingPeriod(rec.Timestamp))

	for range usageCASRetries {
		var current UsageCounters
		var revision uint64
		entry, err := kv.Get(ctx, key)
		switch {
		case err == nil:
			if uerr := json.Unmarshal(entry.Value(), &current); uerr != nil {
				return fmt.Errorf("decode usage counters for %s: %w", key, uerr)
			}
			revision = entry.Revision()
		case errors.Is(err, jetstream.ErrKeyNotFound):
			// Zero value at revision 0: the CAS write below creates it.
		default:
			return fmt.Errorf("kv get usage counters for %s: %w", key, err)
		}

		next := current
		next.InputTokens += rec.InputTokens
		next.OutputTokens += rec.OutputTokens
		next.RequestCount++
		if price.Known {
			next.CostMicroUSD += microUSDCost(rec.InputTokens, price.InputMicroUSDPerMillion) + microUSDCost(rec.OutputTokens, price.OutputMicroUSDPerMillion)
		} else {
			next.CostUnknown = true
		}

		data, merr := json.Marshal(next)
		if merr != nil {
			return fmt.Errorf("encode usage counters for %s: %w", key, merr)
		}

		if revision == 0 {
			_, err = kv.Create(ctx, key, data)
		} else {
			_, err = kv.Update(ctx, key, data, revision)
		}
		if err == nil {
			return nil
		}
		if !isUsageCASConflict(err) {
			return fmt.Errorf("kv write usage counters for %s: %w", key, err)
		}
	}
	return fmt.Errorf("usage counter CAS exhausted for %s after %d attempts", key, usageCASRetries)
}

// TokensThisPeriod sums accountID's input+output tokens across every model
// for the current billing period, for the tokens-per-month quota dimension.
// A few seconds of staleness against the usage consumer is acceptable for a
// monthly cap (see handlers_quota.CheckBedrockTokens).
func (s *UsageStore) TokensThisPeriod(ctx context.Context, accountID string) (int64, error) {
	kv, err := s.bucket(ctx)
	if err != nil {
		return 0, err
	}
	keys, err := kvutil.Keys(ctx, kv)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("list usage keys for %s: %w", accountID, err)
	}

	prefix := accountID + "/"
	suffix := "/" + billingPeriod(time.Now())
	var total int64
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, suffix) {
			continue
		}
		entry, err := kv.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			return 0, fmt.Errorf("kv get usage counters for %s: %w", key, err)
		}
		var counters UsageCounters
		if uerr := json.Unmarshal(entry.Value(), &counters); uerr != nil {
			return 0, fmt.Errorf("decode usage counters for %s: %w", key, uerr)
		}
		total += counters.InputTokens + counters.OutputTokens
	}
	return total, nil
}

// isUsageCASConflict reports whether err is a lost optimistic-concurrency
// race on the usage counter: a Create on an existing key or an Update
// against a stale revision. Mirrors handlers_quota.isCASConflict — the two
// packages don't share an import for this one four-line check.
func isUsageCASConflict(err error) bool {
	if errors.Is(err, jetstream.ErrKeyExists) {
		return true
	}
	if apiErr, ok := errors.AsType[*jetstream.APIError](err); ok {
		return apiErr.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequence
	}
	return false
}

// EnsureUsageConsumer idempotently creates (or updates) this package's
// durable pull consumer for usage/cost metering on the invocation stream.
func EnsureUsageConsumer(ctx context.Context, js jetstream.JetStream) (jetstream.Consumer, error) {
	consumer, err := js.CreateOrUpdateConsumer(ctx, InvocationStreamName, jetstream.ConsumerConfig{
		Durable:       UsageConsumerName,
		FilterSubject: InvocationStreamSubject,
		AckPolicy:     jetstream.AckExplicitPolicy,
		MaxDeliver:    5,
		AckWait:       time.Minute,
	})
	if err != nil {
		return nil, fmt.Errorf("ensure usage metering consumer: %w", err)
	}
	return consumer, nil
}

// UsageConsumer drains UsageConsumerName, dedupes on RequestID, and applies
// each record's token counts and priced cost to its account/model/period
// counter.
type UsageConsumer struct {
	usage  *UsageStore
	prices PriceResolver
}

// NewUsageConsumer constructs a UsageConsumer. prices may be nil, in which
// case every provider entry with no in-tree default price resolves unknown
// (only the in-tree defaults are ever consulted).
func NewUsageConsumer(usage *UsageStore, prices PriceResolver) *UsageConsumer {
	return &UsageConsumer{usage: usage, prices: prices}
}

// Run pulls records from consumer until ctx is cancelled, acking each after
// a successful apply (or a confirmed duplicate) and nak'ing on failure so
// JetStream redelivers it.
func (c *UsageConsumer) Run(ctx context.Context, consumer jetstream.Consumer) {
	iter, err := consumer.Messages()
	if err != nil {
		slog.Error("bedrock: failed to start usage metering consumer", "err", err)
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
			slog.Error("bedrock: usage metering consumer fetch failed", "err", err)
			continue
		}
		c.apply(ctx, msg)
	}
}

// apply decodes, dedupes, applies, and acks (or naks) one message. A decode
// failure acks and drops the message outright: it can never succeed on
// redelivery.
func (c *UsageConsumer) apply(ctx context.Context, msg jetstream.Msg) {
	var rec InvocationRecord
	if err := json.Unmarshal(msg.Data(), &rec); err != nil {
		slog.Error("bedrock: usage metering consumer: undecodable record, dropping", "err", err)
		if ackErr := msg.Ack(); ackErr != nil {
			slog.Error("bedrock: usage metering consumer: ack failed", "err", ackErr)
		}
		return
	}

	first, err := c.usage.MarkProcessed(ctx, rec.RequestID)
	if err != nil {
		slog.Error("bedrock: usage metering consumer: dedupe check failed", "request_id", rec.RequestID, "err", err)
		if nakErr := msg.Nak(); nakErr != nil {
			slog.Error("bedrock: usage metering consumer: nak failed", "err", nakErr)
		}
		return
	}
	if !first {
		// Redelivery of a record this consumer already applied: ack without
		// touching the counter, or a customer gets billed twice.
		slog.Debug("bedrock: usage metering consumer: duplicate record, skipping", "request_id", rec.RequestID)
		if ackErr := msg.Ack(); ackErr != nil {
			slog.Error("bedrock: usage metering consumer: ack failed", "request_id", rec.RequestID, "err", ackErr)
		}
		return
	}

	entry, ok := lookupCatalogEntry(rec.ModelID)
	if !ok {
		// A record for a model no longer in the catalog: still count tokens
		// (they were genuinely consumed) but the cost can never be known.
		slog.Warn("bedrock: usage metering consumer: record for unknown model, cost unresolved", "request_id", rec.RequestID, "model", rec.ModelID)
		if err := c.usage.Apply(ctx, rec, Price{}); err != nil {
			slog.Error("bedrock: usage metering consumer: apply failed", "request_id", rec.RequestID, "err", err)
			if nakErr := msg.Nak(); nakErr != nil {
				slog.Error("bedrock: usage metering consumer: nak failed", "err", nakErr)
			}
			return
		}
		if ackErr := msg.Ack(); ackErr != nil {
			slog.Error("bedrock: usage metering consumer: ack failed", "request_id", rec.RequestID, "err", ackErr)
		}
		return
	}

	price, err := resolvePrice(ctx, c.prices, entry)
	if err != nil {
		slog.Error("bedrock: usage metering consumer: price resolve failed", "request_id", rec.RequestID, "model", rec.ModelID, "err", err)
		if nakErr := msg.Nak(); nakErr != nil {
			slog.Error("bedrock: usage metering consumer: nak failed", "err", nakErr)
		}
		return
	}

	if err := c.usage.Apply(ctx, rec, price); err != nil {
		slog.Error("bedrock: usage metering consumer: apply failed", "request_id", rec.RequestID, "err", err)
		if nakErr := msg.Nak(); nakErr != nil {
			slog.Error("bedrock: usage metering consumer: nak failed", "err", nakErr)
		}
		return
	}
	if err := msg.Ack(); err != nil {
		slog.Error("bedrock: usage metering consumer: ack failed", "request_id", rec.RequestID, "err", err)
	}
}
