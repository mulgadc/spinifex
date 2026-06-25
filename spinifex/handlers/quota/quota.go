// Package handlers_quota enforces per-account service quotas in the AWS gateway.
// It caps how much standing infrastructure a single account can hold (vCPUs,
// VPCs, subnets, Elastic IPs, EBS storage). Limits are a single config-tunable
// tier; the system account and disabled configs bypass every check.
package handlers_quota

import (
	"time"

	"github.com/mulgadc/spinifex/spinifex/utils"
)

// ReconcileInterval is how often the gateway recomputes vCPU counters. It is
// fixed rather than configurable to keep the [quota] block simple.
const ReconcileInterval = 30 * time.Second

// Limits mirrors the [quota] block in awsgw.toml. The zero value (Enabled
// false) is a valid no-op, so gateways without a [quota] block are unaffected.
type Limits struct {
	Enabled    bool `toml:"enabled"`
	VCPUs      int  `toml:"vcpus"`
	VPCs       int  `toml:"vpcs"`
	Subnets    int  `toml:"subnets"`
	EIPs       int  `toml:"eips"`
	VolumesGiB int  `toml:"volumes_gib"`
}

// QuotaService is the per-account quota enforcement surface used by the gateway.
// Enforcement methods are added as the subsystem is built out; for now it is
// just the exemption gate every check short-circuits on.
type QuotaService interface {
	// Exempt reports whether quota checks should be skipped for accountID.
	Exempt(accountID string) bool
}

// Service enforces quotas for one gateway. The vCPU counter, KV handle, and
// instance-type catalog are wired in later steps; for now it holds only the
// configured limits.
type Service struct {
	limits Limits
}

var _ QuotaService = (*Service)(nil)

// New constructs a quota Service from the configured limits.
func New(limits Limits) *Service {
	return &Service{limits: limits}
}

// Exempt returns true for the global/system account and whenever quotas are
// disabled, so those callers bypass every quota check.
func (s *Service) Exempt(accountID string) bool {
	if !s.limits.Enabled {
		return true
	}
	return accountID == utils.GlobalAccountID
}
