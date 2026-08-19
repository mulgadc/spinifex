// Package handlers_quota enforces per-account service quotas in the AWS gateway.
// It caps how much standing infrastructure a single account can hold (vCPUs,
// VPCs, subnets, Elastic IPs, EBS storage). Limits are a single config-tunable
// tier; the system account and disabled configs bypass every check.
package handlers_quota

import (
	"time"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go/jetstream"
)

// ReconcileInterval is how often the gateway recomputes vCPU counters. It is
// fixed rather than configurable to keep the [quota] block simple.
const ReconcileInterval = 30 * time.Second

// KVBucketAccountUsage is the gateway-owned KV bucket holding one integer vCPU
// counter per account, keyed by accountID. It is the only counter-backed
// dimension; the live-counted dimensions need no stored state.
const KVBucketAccountUsage = "spinifex-account-usage"

// KVBucketQuotaReconcile is the leader-lock bucket for the gateway quota
// reconcile. It is deliberately distinct from vpcd's network-reconcile lock so
// the two independent loops never share one mutex and block each other.
const KVBucketQuotaReconcile = "spinifex-quota-reconcile"

// KVBucketAccountQuota holds the per-account limit overrides, keyed by
// accountID. It is separate from the accounts bucket so a quota write never
// contends with an identity write on the same revision.
const KVBucketAccountQuota = "spinifex-account-quota"

// Unlimited disables one dimension for one account. Zero is a real limit that
// denies every request, so "no cap" needs a value of its own.
const Unlimited = -1

// Limits mirrors the [quota] block in awsgw.toml. The zero value (Enabled
// false) is a valid no-op, so gateways without a [quota] block are unaffected.
//
// TokensPerMonthEnabled and RequestsPerMinuteEnabled are deliberately
// separate from Enabled above: a deployment already
// enforcing standing-infra quotas (Enabled=true for VCPUs/VPCs/...) must opt
// in to Bedrock token/RPM enforcement independently, since a bare Enabled
// would otherwise silently cap Bedrock usage at zero the moment any other
// dimension turns on. Both default to disabled, matching the block's
// existing opt-in convention.
type Limits struct {
	Enabled    bool `toml:"enabled"`
	VCPUs      int  `toml:"vcpus"`
	VPCs       int  `toml:"vpcs"`
	Subnets    int  `toml:"subnets"`
	EIPs       int  `toml:"eips"`
	VolumesGiB int  `toml:"volumes_gib"`

	// Volumes caps the number of EBS volumes, independently of VolumesGiB: a
	// tenant can exhaust per-volume overhead long before it exhausts capacity.
	Volumes       int `toml:"volumes"`
	RDSInstances  int `toml:"rds_instances"`
	LoadBalancers int `toml:"load_balancers"`

	TokensPerMonthEnabled    bool  `toml:"tokens_per_month_enabled"`
	TokensPerMonth           int64 `toml:"tokens_per_month"`
	RequestsPerMinuteEnabled bool  `toml:"requests_per_minute_enabled"`
	RequestsPerMinute        int   `toml:"requests_per_minute"`
}

// Service enforces per-account quotas for one gateway. It holds the configured
// limits and the per-account vCPU counter bucket; the live-counted dimensions
// derive their usage from account-filtered Describe* calls and need no state.
type Service struct {
	limits Limits
	// usage holds the per-account vCPU counters (key {accountID}). Nil when
	// quotas are disabled, in which case Exempt short-circuits every check
	// before the counter is touched.
	usage jetstream.KeyValue

	// bedrockUsage resolves the stream-fed Bedrock token counter
	// Nil until SetBedrockUsage is called, in which case
	// CheckBedrockTokens treats the dimension as unconfigured rather than
	// erroring — a wiring gap must not block every Bedrock call.
	bedrockUsage BedrockUsageReader

	// rpm holds the local, per-gateway, per-account token buckets enforcing
	// the requests-per-minute dimension. Always non-nil so CheckBedrockRPM
	// never needs a nil guard beyond Service itself.
	rpm *rpmLimiter

	// overrides holds the per-account limit overrides. Nil leaves every account
	// on the configured limits, which is also what an absent key means.
	overrides jetstream.KeyValue
}

// New constructs a quota Service from the configured limits and the gateway-owned
// account-usage KV bucket. usage may be nil when quotas are disabled; Exempt then
// short-circuits every check before the counter is read.
func New(limits Limits, usage jetstream.KeyValue) *Service {
	return &Service{limits: limits, usage: usage, rpm: newRPMLimiter()}
}

// SetOverrides attaches the per-account override bucket. Called once at wiring
// time; a nil Service or bucket leaves every account on the configured limits.
func (s *Service) SetOverrides(kv jetstream.KeyValue) {
	if s == nil {
		return
	}
	s.overrides = kv
}

// Exempt returns true for the global/system account and whenever quotas are
// disabled, so those callers bypass every quota check. A nil Service (quotas
// never built) is treated as disabled so callers need no nil guard.
func (s *Service) Exempt(accountID string) bool {
	if s == nil || !s.limits.Enabled {
		return true
	}
	return accountID == utils.GlobalAccountID
}
