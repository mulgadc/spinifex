package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go/jetstream"
)

// Overrides raises or lowers individual dimensions for one account. Every field
// is a pointer so an explicit 0 (a limit that denies everything) is distinct
// from an absent field, which inherits the configured value. This is also the
// admin wire type: an admin request body can carry only these fields, so it can
// never set or clear a stored record's state.
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

// OverrideState is a stored override record's lifecycle state. "" is active;
// a record written before the state field existed has none and stays active.
type OverrideState string

// OverrideStateSealed marks a record no writer may change or recreate.
const OverrideStateSealed OverrideState = "sealed"

// overrideRecord is the stored form: the admin-settable fields inline, plus a
// state no admin request can read or write.
type overrideRecord struct {
	Overrides

	State OverrideState `json:"state,omitempty"`
}

// errOverridesSealed marks a read or write that found a sealed record, so a
// caller maps it to Blocked without logging it as a fault.
var errOverridesSealed = errors.New(awserrors.ErrorBlocked)

// errOverridesContended marks a write whose CAS retries were exhausted under
// concurrent writers, bounding the retry loop instead of spinning.
var errOverridesContended = errors.New("quota override: CAS retries exhausted")

// overrideMaxAttempts bounds a write's retries against a revision that keeps
// moving under concurrent writers, so it fails closed instead of spinning.
const overrideMaxAttempts = 5

// checkActive rejects a sealed or unknown-state record; "" is active and
// passes through. A caller must never read or mutate a record that failed
// this as if it were active.
func checkActive(rec overrideRecord) error {
	switch rec.State {
	case "":
		return nil
	case OverrideStateSealed:
		return errOverridesSealed
	default:
		return fmt.Errorf("quota override: unknown state %q", rec.State)
	}
}

// mapOverrideError turns a recordFor, PutAccountQuota or clear failure into
// the AWS error a caller returns: sealed is an expected, unlogged Blocked;
// everything else is InternalError, logged with the account ID.
func mapOverrideError(ctx context.Context, accountID string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errOverridesSealed):
		return errors.New(awserrors.ErrorBlocked)
	case errors.Is(err, errOverridesContended):
		slog.ErrorContext(ctx, "Quota override CAS retries exhausted under contention", "accountID", accountID)
	default:
		slog.ErrorContext(ctx, "Quota override operation failed", "accountID", accountID, "err", err)
	}
	return errors.New(awserrors.ErrorInternalError)
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

// Empty reports whether no dimension is overridden, in which case the record
// carries nothing but provenance and the account resolves to the config.
func (o Overrides) Empty() bool {
	return o.VCPUs == nil && o.VPCs == nil && o.Subnets == nil && o.EIPs == nil &&
		o.VolumesGiB == nil && o.Volumes == nil && o.RDSInstances == nil && o.LoadBalancers == nil
}

// recordFor reads and decodes accountID's stored override record. An absent
// key is the active zero value, not an error; only a genuine read or decode
// failure is returned.
func (s *Service) recordFor(ctx context.Context, accountID string) (overrideRecord, error) {
	if s == nil || s.overrides == nil {
		return overrideRecord{}, nil
	}
	entry, err := s.overrides.Get(ctx, accountID)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return overrideRecord{}, nil
	}
	if err != nil {
		return overrideRecord{}, fmt.Errorf("read quota override for %s: %w", accountID, err)
	}
	var rec overrideRecord
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		return overrideRecord{}, fmt.Errorf("decode quota override for %s: %w", accountID, err)
	}
	return rec, nil
}

// limitsFor resolves accountID's effective limits: overrides where set, the
// config elsewhere. A read or decode failure falls back to the config so an
// outage cannot lift a cap; a sealed or unknown-state record refuses instead.
func (s *Service) limitsFor(ctx context.Context, accountID string) (Limits, error) {
	rec, err := s.recordFor(ctx, accountID)
	if err != nil {
		slog.WarnContext(ctx, "quota: override read failed, using configured limits",
			"account", accountID, "err", err)
		return s.limits, nil
	}
	if err := checkActive(rec); err != nil {
		return Limits{}, mapOverrideError(ctx, accountID, err)
	}
	return rec.apply(s.limits), nil
}

// GetAccountQuota returns accountID's stored overrides and resolved limits.
// Read failures surface rather than show the fallback as real; a sealed or
// unknown-state record returns Blocked or InternalError.
func (s *Service) GetAccountQuota(ctx context.Context, accountID string) (Overrides, Limits, error) {
	rec, err := s.recordFor(ctx, accountID)
	if err != nil {
		return Overrides{}, Limits{}, err
	}
	if err := checkActive(rec); err != nil {
		return Overrides{}, Limits{}, mapOverrideError(ctx, accountID, err)
	}
	return rec.Overrides, rec.apply(s.limits), nil
}

// PutAccountQuota stores or clears accountID's overrides, stamping provenance
// on a store. Both branches are revision-guarded and refuse a sealed record,
// so a seal landing between a read and a write fails the CAS.
func (s *Service) PutAccountQuota(ctx context.Context, accountID string, over Overrides, updatedBy string) error {
	if s == nil || s.overrides == nil {
		return errors.New("quota: override bucket not configured")
	}
	if over.Empty() {
		return s.clearAccountQuota(ctx, accountID)
	}

	over.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	over.UpdatedBy = updatedBy

	_, err := kvutil.Update(ctx, s.overrides, accountID, kvutil.CASConfig{
		CreateIfAbsent: true,
		Attempts:       overrideMaxAttempts,
		Exhausted:      func(string, int) error { return errOverridesContended },
	}, func(rec *overrideRecord) (bool, error) {
		if err := checkActive(*rec); err != nil {
			return false, err
		}
		rec.Overrides = over
		// Every success is a leader-accepted write over an active value: a
		// seal landing between the read and the write fails this CAS.
		return true, nil
	})
	return mapOverrideError(ctx, accountID, err)
}

// clearAccountQuota deletes an active override at the revision it read. An
// observed-absent key writes nothing, so a stale read cannot unseal; a sealed
// record refuses rather than being erased and later recreated active.
func (s *Service) clearAccountQuota(ctx context.Context, accountID string) error {
	for range overrideMaxAttempts {
		entry, err := s.overrides.Get(ctx, accountID)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil
		}
		if err != nil {
			return mapOverrideError(ctx, accountID, fmt.Errorf("read quota override for clear: %w", err))
		}

		var rec overrideRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			return mapOverrideError(ctx, accountID, fmt.Errorf("decode quota override for clear: %w", err))
		}
		if err := checkActive(rec); err != nil {
			return mapOverrideError(ctx, accountID, err)
		}

		err = s.overrides.Delete(ctx, accountID, jetstream.LastRevision(entry.Revision()))
		switch {
		case err == nil:
			return nil
		case errors.Is(err, jetstream.ErrKeyNotFound):
			return nil
		case isRevisionConflict(err):
			// A concurrent write landed between the read and the guarded
			// delete; retry against whatever revision now holds.
			continue
		default:
			return mapOverrideError(ctx, accountID, fmt.Errorf("clear quota override: %w", err))
		}
	}
	return mapOverrideError(ctx, accountID, errOverridesContended)
}
