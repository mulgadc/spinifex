package handlers_quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Overrides raises or lowers individual dimensions for one account. Every field
// is a pointer so an explicit 0 (a limit that denies everything) is distinct
// from an absent field, which inherits the configured value.
type Overrides struct {
	VCPUs         *int `json:"vcpus,omitempty"`
	VPCs          *int `json:"vpcs,omitempty"`
	Subnets       *int `json:"subnets,omitempty"`
	EIPs          *int `json:"eips,omitempty"`
	VolumesGiB    *int `json:"volumes_gib,omitempty"`
	Volumes       *int `json:"volumes,omitempty"`
	RDSInstances  *int `json:"rds_instances,omitempty"`
	LoadBalancers *int `json:"load_balancers,omitempty"`

	// Raising a customer's quota is a commercial act, so it is attributable.
	UpdatedAt string `json:"updated_at,omitempty"`
	UpdatedBy string `json:"updated_by,omitempty"`
}

// Empty reports whether no dimension is overridden, in which case the record
// carries nothing but provenance and the account resolves to the config.
func (o Overrides) Empty() bool {
	return o.VCPUs == nil && o.VPCs == nil && o.Subnets == nil && o.EIPs == nil &&
		o.VolumesGiB == nil && o.Volumes == nil && o.RDSInstances == nil && o.LoadBalancers == nil
}

// apply overlays the set fields onto base, leaving Enabled and the Bedrock
// dimensions alone: enablement is a cluster-wide switch, never per-account.
func (o Overrides) apply(base Limits) Limits {
	out := base
	for _, f := range []struct {
		src *int
		dst *int
	}{
		{o.VCPUs, &out.VCPUs},
		{o.VPCs, &out.VPCs},
		{o.Subnets, &out.Subnets},
		{o.EIPs, &out.EIPs},
		{o.VolumesGiB, &out.VolumesGiB},
		{o.Volumes, &out.Volumes},
		{o.RDSInstances, &out.RDSInstances},
		{o.LoadBalancers, &out.LoadBalancers},
	} {
		if f.src != nil {
			*f.dst = *f.src
		}
	}
	return out
}

// limitsFor resolves the effective limits for accountID: its overrides where
// set, the configured limits everywhere else. A read failure falls back to the
// configured limits rather than to no limit, so a KV outage cannot lift a cap.
func (s *Service) limitsFor(ctx context.Context, accountID string) Limits {
	over, err := s.overridesFor(ctx, accountID)
	if err != nil {
		slog.WarnContext(ctx, "quota: override read failed, using configured limits",
			"account", accountID, "err", err)
		return s.limits
	}
	return over.apply(s.limits)
}

// overridesFor reads accountID's override record. An absent key is the normal
// case for nearly every account and yields the zero value, not an error; only a
// genuine read or decode failure is returned.
func (s *Service) overridesFor(ctx context.Context, accountID string) (Overrides, error) {
	if s == nil || s.overrides == nil {
		return Overrides{}, nil
	}
	entry, err := s.overrides.Get(ctx, accountID)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return Overrides{}, nil
	}
	if err != nil {
		return Overrides{}, fmt.Errorf("read quota override for %s: %w", accountID, err)
	}
	var over Overrides
	if err := json.Unmarshal(entry.Value(), &over); err != nil {
		return Overrides{}, fmt.Errorf("decode quota override for %s: %w", accountID, err)
	}
	return over, nil
}

// GetAccountQuota returns accountID's stored overrides and the limits they
// resolve to. A read failure is surfaced here rather than masked, because an
// operator inspecting a quota must not be shown the fallback as if it were real.
func (s *Service) GetAccountQuota(ctx context.Context, accountID string) (Overrides, Limits, error) {
	over, err := s.overridesFor(ctx, accountID)
	if err != nil {
		return Overrides{}, Limits{}, err
	}
	return over, over.apply(s.limits), nil
}

// PutAccountQuota stores accountID's overrides, stamping provenance. An empty
// set deletes the record so the account resolves to the config, which keeps
// "has this account been touched?" a single existence check.
func (s *Service) PutAccountQuota(ctx context.Context, accountID string, over Overrides, updatedBy string) error {
	if s == nil || s.overrides == nil {
		return errors.New("quota: override bucket not configured")
	}
	if over.Empty() {
		if err := s.overrides.Delete(ctx, accountID); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
			return fmt.Errorf("clear quota override for %s: %w", accountID, err)
		}
		return nil
	}
	over.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	over.UpdatedBy = updatedBy
	data, err := json.Marshal(over)
	if err != nil {
		return fmt.Errorf("encode quota override for %s: %w", accountID, err)
	}
	if _, err := s.overrides.Put(ctx, accountID, data); err != nil {
		return fmt.Errorf("store quota override for %s: %w", accountID, err)
	}
	return nil
}
