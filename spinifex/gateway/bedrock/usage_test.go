package gateway_bedrock

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	usageTestSelfHostModelID = "meta.llama3-2-1b-instruct-v1:0"
	usageTestProviderModelID = "anthropic.claude-3-5-sonnet-20240620-v1:0"
	usageTestAccountA        = "acct-a"
	usageTestAccountB        = "acct-b"
)

func newTestUsageStore(t *testing.T) *UsageStore {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	return NewUsageStore(testutil.NewJetStream(t, nc), 1)
}

// TestMicroUSDCost_Arithmetic covers D12's integer accrual, including
// truncation for token counts too small to register a whole micro-USD.
func TestMicroUSDCost_Arithmetic(t *testing.T) {
	tests := []struct {
		name            string
		tokens          int64
		pricePerMillion int64
		want            int64
	}{
		{"one million tokens at $3/MTok", 1_000_000, 3_000_000, 3_000_000},
		{"1.5 million tokens", 1_500_000, 3_000_000, 4_500_000},
		{"zero tokens", 0, 3_000_000, 0},
		{"sub-unit truncates toward zero", 1, 3_000_000, 3},
		{"one token at $1/MTok truncates to zero", 1, 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, microUSDCost(tt.tokens, tt.pricePerMillion))
		})
	}
}

// TestUsageStore_Apply_AccumulatesAcrossCalls covers the core counter shape:
// two records for the same account/model/period sum tokens, request count,
// and cost rather than overwriting.
func TestUsageStore_Apply_AccumulatesAcrossCalls(t *testing.T) {
	store := newTestUsageStore(t)
	ctx := context.Background()
	now := time.Now()
	price := Price{InputMicroUSDPerMillion: 3_000_000, OutputMicroUSDPerMillion: 15_000_000, Known: true}

	rec1 := InvocationRecord{AccountID: usageTestAccountA, ModelID: usageTestProviderModelID, Timestamp: now, InputTokens: 1_000_000, OutputTokens: 500_000}
	rec2 := InvocationRecord{AccountID: usageTestAccountA, ModelID: usageTestProviderModelID, Timestamp: now, InputTokens: 2_000_000, OutputTokens: 1_000_000}

	require.NoError(t, store.Apply(ctx, rec1, price))
	require.NoError(t, store.Apply(ctx, rec2, price))

	counters, ok, err := store.Get(ctx, usageTestAccountA, usageTestProviderModelID, billingPeriod(now))
	require.NoError(t, err)
	require.True(t, ok)
	assert.EqualValues(t, 3_000_000, counters.InputTokens)
	assert.EqualValues(t, 1_500_000, counters.OutputTokens)
	assert.EqualValues(t, 2, counters.RequestCount)
	// (1M*3 + 0.5M*15) + (2M*3 + 1M*15) = (3M + 7.5M) + (6M + 15M) = 31.5M
	assert.EqualValues(t, 31_500_000, counters.CostMicroUSD)
	assert.False(t, counters.CostUnknown)
}

// TestUsageStore_Apply_SelfHostZeroCostTokensRecorded is the D12 self-host
// rule under test: tokens are recorded exactly like a paid model, but cost
// accrues a *known* zero, never a flag-less absence.
func TestUsageStore_Apply_SelfHostZeroCostTokensRecorded(t *testing.T) {
	store := newTestUsageStore(t)
	ctx := context.Background()
	now := time.Now()

	entry, ok := lookupCatalogEntry(usageTestSelfHostModelID)
	require.True(t, ok)
	price, err := resolvePrice(ctx, nil, entry)
	require.NoError(t, err)

	rec := InvocationRecord{AccountID: usageTestAccountA, ModelID: usageTestSelfHostModelID, Timestamp: now, InputTokens: 42, OutputTokens: 17}
	require.NoError(t, store.Apply(ctx, rec, price))

	counters, ok, err := store.Get(ctx, usageTestAccountA, usageTestSelfHostModelID, billingPeriod(now))
	require.NoError(t, err)
	require.True(t, ok)
	assert.EqualValues(t, 42, counters.InputTokens)
	assert.EqualValues(t, 17, counters.OutputTokens)
	assert.EqualValues(t, 0, counters.CostMicroUSD)
	assert.False(t, counters.CostUnknown, "self-host must accrue a KNOWN zero, not unknown")
}

// TestUsageStore_Apply_UnknownPriceFlagsCostUnknown_NotZero is the other half
// of the same invariant: an unresolvable price must never look like a $0
// period. CostMicroUSD stays whatever it already was; CostUnknown is the only
// signal a reader may trust.
func TestUsageStore_Apply_UnknownPriceFlagsCostUnknown_NotZero(t *testing.T) {
	store := newTestUsageStore(t)
	ctx := context.Background()
	now := time.Now()

	rec := InvocationRecord{AccountID: usageTestAccountA, ModelID: usageTestProviderModelID, Timestamp: now, InputTokens: 100, OutputTokens: 50}
	require.NoError(t, store.Apply(ctx, rec, Price{})) // Known: false

	counters, ok, err := store.Get(ctx, usageTestAccountA, usageTestProviderModelID, billingPeriod(now))
	require.NoError(t, err)
	require.True(t, ok)
	assert.EqualValues(t, 100, counters.InputTokens)
	assert.EqualValues(t, 50, counters.OutputTokens)
	assert.EqualValues(t, 0, counters.CostMicroUSD)
	assert.True(t, counters.CostUnknown)

	// A later record with a KNOWN price must not clear the flag: the period's
	// total is permanently missing the earlier unpriced contribution.
	require.NoError(t, store.Apply(ctx, rec, Price{InputMicroUSDPerMillion: 3_000_000, OutputMicroUSDPerMillion: 15_000_000, Known: true}))
	counters, ok, err = store.Get(ctx, usageTestAccountA, usageTestProviderModelID, billingPeriod(now))
	require.NoError(t, err)
	require.True(t, ok)
	assert.True(t, counters.CostUnknown, "CostUnknown must stay sticky once set")
	assert.Positive(t, counters.CostMicroUSD, "the priced record's cost is still added")
}

// TestUsageStore_MarkProcessed_DedupesRedelivery is the unit-level half of
// the mandatory dedupe: the same RequestID may only be claimed once.
func TestUsageStore_MarkProcessed_DedupesRedelivery(t *testing.T) {
	store := newTestUsageStore(t)
	ctx := context.Background()

	first, err := store.MarkProcessed(ctx, "req-1")
	require.NoError(t, err)
	assert.True(t, first)

	second, err := store.MarkProcessed(ctx, "req-1")
	require.NoError(t, err)
	assert.False(t, second, "a redelivered RequestID must not be claimed twice")

	// A distinct RequestID is unaffected.
	third, err := store.MarkProcessed(ctx, "req-2")
	require.NoError(t, err)
	assert.True(t, third)
}

// TestUsageStore_TokensThisPeriod_SumsAcrossModels covers the reader the
// tokens-per-month quota dimension consults: total is input+output summed
// across every model for the account, in the current period only.
func TestUsageStore_TokensThisPeriod_SumsAcrossModels(t *testing.T) {
	store := newTestUsageStore(t)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, store.Apply(ctx, InvocationRecord{AccountID: usageTestAccountA, ModelID: usageTestSelfHostModelID, Timestamp: now, InputTokens: 100, OutputTokens: 50}, Price{Known: true}))
	require.NoError(t, store.Apply(ctx, InvocationRecord{AccountID: usageTestAccountA, ModelID: usageTestProviderModelID, Timestamp: now, InputTokens: 200, OutputTokens: 25}, Price{Known: true}))
	// A different account's usage must not bleed into accountA's total.
	require.NoError(t, store.Apply(ctx, InvocationRecord{AccountID: usageTestAccountB, ModelID: usageTestSelfHostModelID, Timestamp: now, InputTokens: 9_999, OutputTokens: 9_999}, Price{Known: true}))

	total, err := store.TokensThisPeriod(ctx, usageTestAccountA)
	require.NoError(t, err)
	assert.EqualValues(t, 100+50+200+25, total)
}

// TestUsageStore_TokensThisPeriod_NoUsageIsZero covers the empty-bucket path
// (jetstream.ErrNoKeysFound) rather than erroring.
func TestUsageStore_TokensThisPeriod_NoUsageIsZero(t *testing.T) {
	store := newTestUsageStore(t)
	total, err := store.TokensThisPeriod(context.Background(), usageTestAccountA)
	require.NoError(t, err)
	assert.Zero(t, total)
}

// TestUsageConsumer_Run_RedeliveredRecordDoesNotDoubleCount is the
// bead-mandated assertion: an at-least-once redelivery of the same
// InvocationRecord — modelled here as the same JSON payload landing on the
// stream twice, the shape a StreamRecorder retry after an ambiguous publish
// outcome takes on the wire — must be applied exactly once.
func TestUsageConsumer_Run_RedeliveredRecordDoesNotDoubleCount(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := EnsureInvocationStream(ctx, js, 1)
	require.NoError(t, err)
	consumer, err := EnsureUsageConsumer(ctx, js)
	require.NoError(t, err)

	now := time.Now()
	rec := InvocationRecord{
		RequestID: "req-redelivered", AccountID: usageTestAccountA, ModelID: usageTestSelfHostModelID,
		Operation: OperationConverse, Timestamp: now, InputTokens: 10, OutputTokens: 5,
	}
	payload, err := json.Marshal(rec)
	require.NoError(t, err)

	// Two distinct stream messages carrying the same RequestID: JetStream's
	// own publish-time msgID dedup only guards a single Publish call, not
	// this scenario, which is exactly why a consumer-side dedupe is
	// mandatory.
	_, err = js.Publish(ctx, InvocationStreamSubject, payload)
	require.NoError(t, err)
	_, err = js.Publish(ctx, InvocationStreamSubject, payload)
	require.NoError(t, err)

	usage := NewUsageStore(js, 1)
	uc := NewUsageConsumer(usage, nil)
	done := make(chan struct{})
	go func() {
		uc.Run(ctx, consumer)
		close(done)
	}()

	period := billingPeriod(now)
	require.Eventually(t, func() bool {
		counters, ok, err := usage.Get(context.Background(), usageTestAccountA, usageTestSelfHostModelID, period)
		return err == nil && ok && counters.RequestCount >= 1
	}, 2*time.Second, 10*time.Millisecond)

	// Give the (already-processed) second message a moment to be consumed
	// and confirmed as a duplicate before asserting the final count.
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	counters, ok, err := usage.Get(context.Background(), usageTestAccountA, usageTestSelfHostModelID, period)
	require.NoError(t, err)
	require.True(t, ok)
	assert.EqualValues(t, 1, counters.RequestCount, "redelivery must not double-count the request")
	assert.EqualValues(t, 10, counters.InputTokens, "redelivery must not double-count tokens")
	assert.EqualValues(t, 5, counters.OutputTokens)
}

// TestUsageConsumer_Run_UnknownModelStillRecordsTokensCostUnknown covers a
// record for a model the catalog no longer knows (e.g. retired since the
// call was made): tokens were genuinely consumed and must still be counted,
// but cost can never be resolved for it.
func TestUsageConsumer_Run_UnknownModelStillRecordsTokensCostUnknown(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := EnsureInvocationStream(ctx, js, 1)
	require.NoError(t, err)
	consumer, err := EnsureUsageConsumer(ctx, js)
	require.NoError(t, err)

	now := time.Now()
	rec := InvocationRecord{
		RequestID: "req-retired-model", AccountID: usageTestAccountA, ModelID: "retired.model-v0:0",
		Operation: OperationConverse, Timestamp: now, InputTokens: 8, OutputTokens: 4,
	}
	payload, err := json.Marshal(rec)
	require.NoError(t, err)
	_, err = js.Publish(ctx, InvocationStreamSubject, payload)
	require.NoError(t, err)

	usage := NewUsageStore(js, 1)
	uc := NewUsageConsumer(usage, nil)
	done := make(chan struct{})
	go func() {
		uc.Run(ctx, consumer)
		close(done)
	}()

	period := billingPeriod(now)
	require.Eventually(t, func() bool {
		_, ok, err := usage.Get(context.Background(), usageTestAccountA, "retired.model-v0:0", period)
		return err == nil && ok
	}, 2*time.Second, 10*time.Millisecond)
	cancel()
	<-done

	counters, ok, err := usage.Get(context.Background(), usageTestAccountA, "retired.model-v0:0", period)
	require.NoError(t, err)
	require.True(t, ok)
	assert.EqualValues(t, 8, counters.InputTokens)
	assert.EqualValues(t, 4, counters.OutputTokens)
	assert.True(t, counters.CostUnknown)
}

// TestBillingPeriod_MonthlyFormat pins the period key shape, since
// TokensThisPeriod's prefix/suffix matching depends on it exactly.
func TestBillingPeriod_MonthlyFormat(t *testing.T) {
	ts := time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)
	assert.Equal(t, "2026-08", billingPeriod(ts))
}

// TestUsageStore_RequiresJetStream covers a store constructed with no
// JetStream client: both the counter bucket and the dedupe bucket must report
// the misconfiguration rather than panic on a nil handle.
func TestUsageStore_RequiresJetStream(t *testing.T) {
	store := NewUsageStore(nil, 1)
	ctx := context.Background()

	_, _, err := store.Get(ctx, "000000000001", "m", "2026-08")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no JetStream client configured")

	_, err = store.MarkProcessed(ctx, "req-1")
	require.Error(t, err)

	require.Error(t, store.Apply(ctx, InvocationRecord{AccountID: "000000000001", ModelID: "m"}, Price{}))

	_, err = store.TokensThisPeriod(ctx, "000000000001")
	require.Error(t, err)
}
