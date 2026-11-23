package ochre

import (
	"errors"
	"fmt"
	"math"

	"github.com/mulgadc/spinifex/spinifex/domains/ochre/readiness"
)

// errLaunchSettlement is a launch worker write that is not one of its three
// settling steps, or that is not the claim's own worker's.
var errLaunchSettlement = errors.New("bedrock: not a launch settlement of an endpoint record")

var _ readiness.SettleCheck = CheckLaunchSettlement

// CheckLaunchSettlement allows the launch worker that owns a claim to record
// its realized bundle, then to park it or publish it READY once. It cannot
// show that the launch or the readiness probe succeeded.
func CheckLaunchSettlement(s readiness.Settlement) error {
	switch {
	case s.Kind != readiness.TokenLaunch || s.TokenID == "" || s.Epoch == 0 || s.Node == "":
		return fmt.Errorf("%w: not an admitted launch token", errLaunchSettlement)
	case !s.Owned:
		return fmt.Errorf("%w: the operation did not write revision %d", errLaunchSettlement, s.Revision)
	case s.Next == nil:
		return fmt.Errorf("%w: a launch settlement never removes a record", errLaunchSettlement)
	case s.Target == "" || s.Target != s.Key:
		return fmt.Errorf("%w: token target %q is not key %q", errLaunchSettlement, s.Target, s.Key)
	}
	old, err := strictRecord(s.Old)
	if err != nil {
		return fmt.Errorf("%w: stored record: %w", errLaunchSettlement, err)
	}
	next, err := strictRecord(s.Next)
	if err != nil {
		return fmt.Errorf("%w: new record: %w", errLaunchSettlement, err)
	}
	if resolveKey(resolveAccountID(old.AccountID), old.ModelID) != s.Key {
		return fmt.Errorf("%w: record does not belong at key %q", errLaunchSettlement, s.Key)
	}
	owner, ok, err := old.Owner()
	switch {
	case err != nil || !ok:
		return fmt.Errorf("%w: record has no valid launch owner", errLaunchSettlement)
	case owner.TokenID != s.TokenID || owner.Epoch != s.Epoch || old.NodeID != s.Node:
		return fmt.Errorf("%w: token is not the record's launch worker", errLaunchSettlement)
	case old.State != StateStarting || old.ParkedReason != "" || !old.ParkedAt.IsZero() ||
		old.HealthHold != nil || !old.ReadyAt.IsZero() || !unobserved(old):
		return fmt.Errorf("%w: record is not an open launch claim", errLaunchSettlement)
	case old.Generation == math.MaxUint64:
		return fmt.Errorf("%w: generation cannot advance", errLaunchSettlement)
	}

	want := old
	if bareClaim(old) {
		want.InstanceID, want.ENIID, want.PrivateIP = next.InstanceID, next.ENIID, next.PrivateIP
		want.WeightsVolumeID, want.Members = next.WeightsVolumeID, next.Members
		if _, err := catalogBundle(want); err != nil {
			return fmt.Errorf("%w: realized bundle: %w", errLaunchSettlement, err)
		}
	} else {
		bundle, err := catalogBundle(old)
		if err != nil {
			return fmt.Errorf("%w: claim is neither bare nor a complete bundle: %w", errLaunchSettlement, err)
		}
		if old.BaseURL != "" {
			return fmt.Errorf("%w: realized claim already has a base URL", errLaunchSettlement)
		}
		switch next.State {
		case StateReady:
			if next.ReadyAt.IsZero() {
				return fmt.Errorf("%w: a READY record needs its ready time", errLaunchSettlement)
			}
			want.State, want.ReadyAt = StateReady, next.ReadyAt
			want.BaseURL = bundle.targets[bundle.primary].BaseURL
		default:
			switch next.ParkedReason {
			case "":
				return fmt.Errorf("%w: a realized claim is only parked or published READY", errLaunchSettlement)
			case ParkReadinessRedirected, ParkReadinessTimedOut:
			default:
				return fmt.Errorf("%w: park reason %q is not known", errLaunchSettlement, next.ParkedReason)
			}
			if next.ParkedAt.IsZero() {
				return fmt.Errorf("%w: a park needs its time", errLaunchSettlement)
			}
			want.ParkedReason, want.ParkedAt = next.ParkedReason, next.ParkedAt
		}
	}
	want.Generation = old.Generation + 1

	same, err := sameEncoding(want, next)
	if err != nil {
		return fmt.Errorf("%w: %w", errLaunchSettlement, err)
	}
	if !same {
		return fmt.Errorf("%w: new record changes more than the launch step allows", errLaunchSettlement)
	}
	return nil
}

// bareClaim reports a STARTING claim as admission wrote it: a creation time
// and node, with no realized resource or address yet.
func bareClaim(rec EndpointRecord) bool {
	return !rec.CreatedAt.IsZero() && rec.NodeID != "" &&
		rec.InstanceID == "" && rec.ENIID == "" && rec.PrivateIP == "" && rec.WeightsVolumeID == "" &&
		rec.Members == nil && rec.BaseURL == ""
}

// unobserved reports a record that carries no reaper observation.
func unobserved(rec EndpointRecord) bool {
	return rec.LastActiveAt.IsZero() && rec.InFlight == 0 && rec.SuccessTotal == 0 && rec.ScrapeFailures == 0
}
