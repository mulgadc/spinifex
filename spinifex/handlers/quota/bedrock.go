package handlers_quota

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go/jetstream"
	"golang.org/x/time/rate"
)

// BedrockUsageReader resolves accountID's current-period Bedrock token usage
// (input+output, across every model), the counter gateway_bedrock's usage
// consumer maintains from the invocation stream. The
// interface is defined here rather than importing gateway_bedrock so the two
// packages stay decoupled; gateway_bedrock.UsageStore satisfies it
// structurally, and awsgw.go wires the two together at startup.
type BedrockUsageReader interface {
	TokensThisPeriod(ctx context.Context, accountID string) (int64, error)
}

// SetBedrockUsage installs the reader CheckBedrockTokens consults. Called
// once at gateway startup; a nil (never-installed) reader leaves the
// dimension exempt even when TokensPerMonthEnabled — a wiring gap must not
// block every Bedrock call.
func (s *Service) SetBedrockUsage(r BedrockUsageReader) {
	s.bedrockUsage = r
}

// CheckBedrockTokens rejects with ServiceQuotaExceededException when
// accountID's current-period token usage has already reached the configured
// monthly cap. Unlike CheckVCPU's check-before-grant shape, a call's token
// cost is only known once it completes, so this is a check-against-history
// gate: the invocation that crosses the cap is itself allowed (its cost
// isn't known until it finishes), and the next one is rejected once the
// stream-fed counter catches up. A few seconds of lag behind real usage is
// an accepted property of a monthly cap.
func (s *Service) CheckBedrockTokens(ctx context.Context, accountID string) error {
	if s == nil || !s.limits.TokensPerMonthEnabled || accountID == utils.GlobalAccountID {
		return nil
	}
	if s.bedrockUsage == nil {
		return nil
	}
	tokens, err := s.bedrockUsage.TokensThisPeriod(ctx, accountID)
	if err != nil {
		return err
	}
	if tokens >= s.limits.TokensPerMonth {
		return errors.New(awserrors.ErrorServiceQuotaExceededException)
	}
	return nil
}

// CheckBedrockRPM rejects with ThrottlingException when accountID has
// exhausted its local per-gateway request-rate bucket. Enforcement is local
// and immediate — it never reads the invocation stream or a shared KV
// counter — which is deliberately generous under multiple gateways (N
// gateways x bucket capacity each): the correct direction to err for a rate
// limit is to occasionally allow a little more, never to add network latency
// to every invocation for a slightly tighter cap.
func (s *Service) CheckBedrockRPM(accountID string) error {
	if s == nil || !s.limits.RequestsPerMinuteEnabled || accountID == utils.GlobalAccountID {
		return nil
	}
	if !s.rpm.allow(accountID, s.limits.RequestsPerMinute) {
		return errors.New(awserrors.ErrorThrottlingException)
	}
	return nil
}

// newTokenBucket constructs a full bucket (a fresh account starts able to
// burst its whole per-minute allowance, not throttled from zero) at
// capacityPerMinute, refilling continuously at capacity/60 per second.
func newTokenBucket(capacityPerMinute int) *rate.Limiter {
	capacity := max(capacityPerMinute, 0)
	return rate.NewLimiter(rate.Limit(float64(capacity)/60), capacity)
}

// rpmLimiter holds one bucket per account, created lazily on first use at the
// configured per-minute capacity. Buckets are never evicted: an idle account
// costs one small map entry, never unbounded growth, and a restart resets
// every bucket to full — acceptable since RPM enforcement is already
// documented as generous, not exact.
type rpmLimiter struct {
	mu      sync.Mutex
	buckets map[string]*rate.Limiter
	// now overrides the clock every bucket is measured against; nil (the
	// production default) means time.Now.
	now func() time.Time
}

func newRPMLimiter() *rpmLimiter {
	return &rpmLimiter{buckets: make(map[string]*rate.Limiter)}
}

// clock returns the limiter's current time, honouring a test override.
func (l *rpmLimiter) clock() time.Time {
	if l.now == nil {
		return time.Now()
	}
	return l.now()
}

// allow returns whether accountID may proceed under capacityPerMinute,
// creating its bucket on first use.
func (l *rpmLimiter) allow(accountID string, capacityPerMinute int) bool {
	return l.bucketFor(accountID, capacityPerMinute).AllowN(l.clock(), 1)
}

// bucketFor returns accountID's bucket, creating it at capacityPerMinute on
// first use. Capacity is fixed at creation: RPM limits are process-lifetime
// config (loaded once at gateway startup), not hot-reloaded.
func (l *rpmLimiter) bucketFor(accountID string, capacityPerMinute int) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[accountID]
	if !ok {
		b = newTokenBucket(capacityPerMinute)
		l.buckets[accountID] = b
	}
	return b
}

// snapshot returns every known account's current bucket occupancy, for the
// periodic KV sync below.
func (l *rpmLimiter) snapshot() map[string]int {
	now := l.clock()
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]int, len(l.buckets))
	for accountID, b := range l.buckets {
		out[accountID] = int(b.TokensAt(now))
	}
	return out
}

// KVBucketBedrockRPMUsage is the gateway-owned KV bucket RunBedrockRPMSync
// writes each account's current bucket occupancy to, keyed by accountID. It
// exists for cross-node visibility and dashboards only: CheckBedrockRPM never
// reads it back. Enforcement must stay local and immediate — reading a
// remote value here would reintroduce exactly the round-trip latency that
// ruled out stream-fed RPM enforcement in the first place. Concurrent
// gateways overwrite each other's snapshot for the same account with no
// coordination, which is fine: this bucket is observational, not
// correctness-bearing.
const KVBucketBedrockRPMUsage = "bedrock-rpm-usage"

// bedrockRPMSyncInterval is the sync cadence, matching the vCPU reconcile's
// ReconcileInterval so the two background loops share one cadence to reason
// about.
const bedrockRPMSyncInterval = ReconcileInterval

// bedrockRPMSnapshot is one account's RPM bucket occupancy at sync time.
type bedrockRPMSnapshot struct {
	Remaining int       `json:"remaining"`
	Capacity  int       `json:"capacity"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// RunBedrockRPMSync periodically snapshots every account this gateway has
// rate-limited into kv, until ctx is cancelled. A no-op when the RPM
// dimension is disabled, so a default-off deployment starts no ticker.
func (s *Service) RunBedrockRPMSync(ctx context.Context, js jetstream.JetStream, replicas int) {
	if s == nil || !s.limits.RequestsPerMinuteEnabled {
		return
	}
	kv, err := kvutil.GetOrCreateBucketWithReplicas(ctx, js, KVBucketBedrockRPMUsage, 1, replicas)
	if err != nil {
		slog.Warn("bedrock rpm sync: open usage bucket failed, sync disabled", "err", err)
		return
	}
	ticker := time.NewTicker(bedrockRPMSyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.syncBedrockRPMOnce(ctx, kv)
		}
	}
}

// syncBedrockRPMOnce writes one snapshot pass of every known account's
// bucket occupancy to kv. A per-account write failure is logged and the pass
// continues, since this data is observational only.
func (s *Service) syncBedrockRPMOnce(ctx context.Context, kv jetstream.KeyValue) {
	for accountID, remaining := range s.rpm.snapshot() {
		data, err := json.Marshal(bedrockRPMSnapshot{
			Remaining: remaining,
			Capacity:  s.limits.RequestsPerMinute,
			UpdatedAt: time.Now(),
		})
		if err != nil {
			slog.Warn("bedrock rpm sync: encode snapshot failed", "account", accountID, "err", err)
			continue
		}
		if _, err := kv.Put(ctx, accountID, data); err != nil {
			slog.Warn("bedrock rpm sync: write failed", "account", accountID, "err", err)
		}
	}
}
