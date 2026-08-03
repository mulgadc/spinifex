package handlers_quota

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go/jetstream"
	toml "github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubBedrockUsageReader returns a fixed (tokens, err) for every account.
type stubBedrockUsageReader struct {
	tokens int64
	err    error
}

func (s stubBedrockUsageReader) TokensThisPeriod(context.Context, string) (int64, error) {
	return s.tokens, s.err
}

// TestCheckBedrockTokens_Boundaries covers the hard cap at, just under, and
// just over the configured monthly limit.
func TestCheckBedrockTokens_Boundaries(t *testing.T) {
	tests := []struct {
		name       string
		tokens     int64
		wantExceed bool
	}{
		{"just under cap passes", 999, false},
		{"at cap rejects", 1000, true},
		{"over cap rejects", 1001, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(Limits{TokensPerMonthEnabled: true, TokensPerMonth: 1000}, nil)
			s.SetBedrockUsage(stubBedrockUsageReader{tokens: tt.tokens})
			err := s.CheckBedrockTokens(context.Background(), testAccount)
			if !tt.wantExceed {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorServiceQuotaExceededException, err.Error())
		})
	}
}

func TestCheckBedrockTokens_DisabledIsNoop(t *testing.T) {
	s := New(Limits{}, nil)
	s.SetBedrockUsage(stubBedrockUsageReader{tokens: 1_000_000_000})
	assert.NoError(t, s.CheckBedrockTokens(context.Background(), testAccount))
}

func TestCheckBedrockTokens_ExemptSystemAccount(t *testing.T) {
	s := New(Limits{TokensPerMonthEnabled: true, TokensPerMonth: 1}, nil)
	s.SetBedrockUsage(stubBedrockUsageReader{tokens: 1_000_000})
	assert.NoError(t, s.CheckBedrockTokens(context.Background(), utils.GlobalAccountID))
}

// TestCheckBedrockTokens_NoReaderIsNoop covers a wiring gap: the dimension is
// enabled but SetBedrockUsage was never called. Failing open here (rather
// than blocking every Bedrock call) is a deliberate choice, distinct from a
// live read failure, which fails closed below.
func TestCheckBedrockTokens_NoReaderIsNoop(t *testing.T) {
	s := New(Limits{TokensPerMonthEnabled: true, TokensPerMonth: 1}, nil)
	assert.NoError(t, s.CheckBedrockTokens(context.Background(), testAccount))
}

// TestCheckBedrockTokens_ReaderErrorPropagates covers a live read failure
// (e.g. the usage KV bucket is unreachable): this fails closed, surfacing
// the error rather than silently allowing the call through.
func TestCheckBedrockTokens_ReaderErrorPropagates(t *testing.T) {
	s := New(Limits{TokensPerMonthEnabled: true, TokensPerMonth: 1000}, nil)
	s.SetBedrockUsage(stubBedrockUsageReader{err: errors.New("boom")})
	err := s.CheckBedrockTokens(context.Background(), testAccount)
	require.Error(t, err)
	assert.Equal(t, "boom", err.Error())
}

// TestCheckBedrockTokens_NilServiceIsNoop mirrors CheckVCPU's nil-safety:
// route handlers call gw.Quota.CheckBedrockTokens unconditionally, and
// gw.Quota is nil in test harnesses/unconfigured gateways.
func TestCheckBedrockTokens_NilServiceIsNoop(t *testing.T) {
	var s *Service
	assert.NoError(t, s.CheckBedrockTokens(context.Background(), testAccount))
}

func TestCheckBedrockRPM_DisabledIsNoop(t *testing.T) {
	s := New(Limits{}, nil)
	for range 1000 {
		assert.NoError(t, s.CheckBedrockRPM(testAccount))
	}
}

func TestCheckBedrockRPM_ExemptSystemAccount(t *testing.T) {
	s := New(Limits{RequestsPerMinuteEnabled: true, RequestsPerMinute: 1}, nil)
	for range 10 {
		assert.NoError(t, s.CheckBedrockRPM(utils.GlobalAccountID))
	}
}

func TestCheckBedrockRPM_NilServiceIsNoop(t *testing.T) {
	var s *Service
	assert.NoError(t, s.CheckBedrockRPM(testAccount))
}

// TestCheckBedrockRPM_ThrottlesAfterCapacityExhausted covers immediate
// enforcement: the (capacity+1)th request within the same instant is
// throttled, and a different account is unaffected.
func TestCheckBedrockRPM_ThrottlesAfterCapacityExhausted(t *testing.T) {
	s := New(Limits{RequestsPerMinuteEnabled: true, RequestsPerMinute: 3}, nil)

	for i := range 3 {
		assert.NoErrorf(t, s.CheckBedrockRPM(testAccount), "request %d within capacity", i)
	}
	err := s.CheckBedrockRPM(testAccount)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorThrottlingException, err.Error())

	// A different account has its own, unexhausted bucket.
	assert.NoError(t, s.CheckBedrockRPM("other-account"))
}

// TestTokenBucket_RefillsOverTime drives the bucket with a fake clock so
// refill behaviour is deterministic rather than sleep-based.
func TestTokenBucket_RefillsOverTime(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	b := newTokenBucket(60, clock) // 60/min == 1/sec

	// Drain the bucket.
	for range 60 {
		require.True(t, b.Allow())
	}
	assert.False(t, b.Allow(), "bucket must be empty immediately after draining")

	// Advance 5 seconds: 5 tokens should have refilled.
	now = now.Add(5 * time.Second)
	for i := range 5 {
		assert.Truef(t, b.Allow(), "refilled token %d", i)
	}
	assert.False(t, b.Allow(), "no more than the refilled amount is available")

	// Advance well past capacity: refill caps at capacity, it never
	// overflows from an idle account banking unlimited tokens.
	now = now.Add(10 * time.Minute)
	assert.Equal(t, 60, b.remaining())
}

// TestRPMLimiter_PerAccountIsolation covers that one account's bucket never
// interferes with another's.
func TestRPMLimiter_PerAccountIsolation(t *testing.T) {
	l := newRPMLimiter()
	assert.True(t, l.allow("a", 1))
	assert.False(t, l.allow("a", 1))
	assert.True(t, l.allow("b", 1))
}

// TestService_SyncBedrockRPMOnce_WritesSnapshotToKV covers the periodic KV
// sync: after consuming from an account's bucket, a sync pass writes its
// current occupancy to KV for cross-node observability.
func TestService_SyncBedrockRPMOnce_WritesSnapshotToKV(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: KVBucketBedrockRPMUsage, History: 1})
	require.NoError(t, err)

	s := New(Limits{RequestsPerMinuteEnabled: true, RequestsPerMinute: 10}, nil)
	require.NoError(t, s.CheckBedrockRPM(testAccount))
	require.NoError(t, s.CheckBedrockRPM(testAccount))

	s.syncBedrockRPMOnce(t.Context(), kv)

	entry, err := kv.Get(t.Context(), testAccount)
	require.NoError(t, err)
	var snapshot bedrockRPMSnapshot
	require.NoError(t, json.Unmarshal(entry.Value(), &snapshot))
	assert.Equal(t, 8, snapshot.Remaining)
	assert.Equal(t, 10, snapshot.Capacity)
}

// TestRunBedrockRPMSync_DisabledNoop asserts the sync loop never touches its
// (here nil) JetStream client when the dimension is disabled — a real
// deployment leaves js valid, but the short-circuit must come first.
func TestRunBedrockRPMSync_DisabledNoop(t *testing.T) {
	s := New(Limits{}, nil)
	done := make(chan struct{})
	go func() {
		s.RunBedrockRPMSync(context.Background(), nil, 1)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunBedrockRPMSync did not return promptly when disabled")
	}
}

// TestLimitsTOMLRoundTrip_BedrockDimensions extends the base round-trip test
// to the two new Ochre dimensions, including confirming they default to
// disabled when absent from the TOML source.
func TestLimitsTOMLRoundTrip_BedrockDimensions(t *testing.T) {
	const src = `
[quota]
enabled                     = true
vcpus                       = 8
tokens_per_month_enabled    = true
tokens_per_month            = 1000000
requests_per_minute_enabled = true
requests_per_minute         = 60
`
	want := Limits{
		Enabled:                  true,
		VCPUs:                    8,
		TokensPerMonthEnabled:    true,
		TokensPerMonth:           1_000_000,
		RequestsPerMinuteEnabled: true,
		RequestsPerMinute:        60,
	}

	var parsed quotaTOML
	require.NoError(t, toml.Unmarshal([]byte(src), &parsed))
	assert.Equal(t, want, parsed.Quota)
}
