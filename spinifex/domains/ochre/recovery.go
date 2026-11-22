package ochre

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
)

// errRecoveryIncomplete marks a parked record whose persisted identity cannot
// be safely compared with what its co-serve group must have launched.
var errRecoveryIncomplete = errors.New("bedrock: parked endpoint identity is incomplete")

// recoverParked re-probes every parked launch, in every account, and promotes
// one to READY only when each expected member answers its own route directly.
// It never terminates, deletes or relaunches anything.
func (r *Reaper) recoverParked(ctx context.Context, lost <-chan struct{}) error {
	recs, err := r.svc.store.listAll(ctx)
	if err != nil {
		return err
	}
	for _, rec := range recs {
		if !rec.Parked() {
			continue
		}
		if !leading(lost) {
			slog.InfoContext(ctx, "bedrock reaper: lease lost; ending parked recovery", "key", r.lease.Key())
			return nil
		}
		r.recoverOne(ctx, rec, lost)
	}
	return nil
}

func (r *Reaper) recoverOne(ctx context.Context, listed EndpointRecord, lost <-chan struct{}) {
	key := resolveKey(resolveAccountID(listed.AccountID), listed.ModelID)
	// Everything below is derived from this read, never from the listing; the
	// promotion is then a CAS at this exact revision.
	observed, rev, found, err := r.svc.store.getRevision(ctx, key)
	if err != nil || !found || !observed.Parked() ||
		resolveKey(resolveAccountID(observed.AccountID), observed.ModelID) != key {
		return
	}
	targets, primaryURL, err := parkedTargets(observed)
	if err != nil {
		slog.WarnContext(ctx, "bedrock reaper: parked endpoint left parked",
			"model", observed.ModelID, "instanceId", observed.InstanceID, "err", err)
		return
	}

	probeCtx, cancel := context.WithTimeout(ctx, r.scrapeTimeout())
	defer cancel()
	for modelID, target := range targets {
		if probeLiveness(probeCtx, r.svc.httpClient(), target.BaseURL+target.Path) != livenessLive {
			slog.InfoContext(ctx, "bedrock reaper: parked endpoint member not ready; left parked",
				"model", observed.ModelID, "member", modelID, "instanceId", observed.InstanceID)
			return
		}
	}

	unlock := r.svc.ensureMu.lock(key)
	defer unlock()
	if !leading(lost) {
		return
	}
	current, currentRev, found, err := r.svc.store.getRevision(ctx, key)
	if err != nil || !found || currentRev != rev || !sameParked(current, observed) {
		slog.InfoContext(ctx, "bedrock reaper: parked endpoint changed during recovery; yielding",
			"model", observed.ModelID, "instanceId", observed.InstanceID)
		return
	}
	if err := validateTransition(StateStarting, StateReady); err != nil {
		return
	}
	next := current
	next.ParkedReason = ""
	next.ParkedAt = time.Time{}
	next.State = StateReady
	next.BaseURL = primaryURL
	next.ReadyAt = time.Now().UTC()
	next.Generation = current.Generation + 1
	if _, err := r.svc.store.CompareAndSet(ctx, key, &next, rev); err != nil {
		if errors.Is(err, kvstore.ErrConflict) {
			slog.InfoContext(ctx, "bedrock reaper: parked endpoint changed during recovery; yielding",
				"model", observed.ModelID, "instanceId", observed.InstanceID)
			return
		}
		slog.ErrorContext(ctx, "bedrock reaper: promoting a recovered endpoint failed",
			"model", observed.ModelID, "instanceId", observed.InstanceID, "err", err)
		return
	}
	slog.InfoContext(ctx, "bedrock reaper: parked endpoint recovered to READY",
		"model", observed.ModelID, "instanceId", observed.InstanceID)
}

// sameParked reports whether current is still the parked incarnation and
// generation observed, with the same reason.
func sameParked(current, observed EndpointRecord) bool {
	return current.Parked() && sameIncarnation(current, observed) &&
		current.Generation == observed.Generation && current.ParkedReason == observed.ParkedReason
}

// parkedTargets checks rec against the co-serve group its model launches with
// and returns each member's readiness target and the primary's base URL. Every
// member must match the catalog's set, family and assigned port exactly.
func parkedTargets(rec EndpointRecord) (map[string]readinessTarget, string, error) {
	switch rec.ParkedReason {
	case ParkReadinessRedirected, ParkReadinessTimedOut:
	default:
		return nil, "", fmt.Errorf("%w: unknown park reason", errRecoveryIncomplete)
	}
	if rec.ParkedAt.IsZero() || rec.CreatedAt.IsZero() || rec.NodeID == "" || rec.InstanceID == "" || rec.ENIID == "" ||
		rec.WeightsVolumeID == "" || net.ParseIP(rec.PrivateIP) == nil {
		return nil, "", fmt.Errorf("%w: missing marker or resource identity", errRecoveryIncomplete)
	}
	spec, found, selfHost := LookupCoServeGroup(rec.ModelID)
	if !found || !selfHost {
		return nil, "", fmt.Errorf("%w: model is not a self-hosted catalog entry", errRecoveryIncomplete)
	}
	if len(rec.Members) != len(spec.Members) {
		return nil, "", fmt.Errorf("%w: member set differs from the catalog", errRecoveryIncomplete)
	}
	inputs := make([]LaunchMemberInput, 0, len(spec.Members))
	for _, m := range spec.Members {
		inputs = append(inputs, LaunchMemberInput{ModelID: m.ModelID, Family: m.Family})
	}
	ports := assignMemberPorts(inputs)

	primary := spec.Members[0].ModelID
	targets := make(map[string]readinessTarget, len(spec.Members))
	seenPorts := make(map[int]bool, len(spec.Members))
	seenVolumes := make(map[string]bool, len(spec.Members))
	for _, want := range spec.Members {
		got, ok := rec.Members[want.ModelID]
		switch {
		case !ok:
			return nil, "", fmt.Errorf("%w: member set differs from the catalog", errRecoveryIncomplete)
		case got.Family != want.Family:
			return nil, "", fmt.Errorf("%w: member family differs from the catalog", errRecoveryIncomplete)
		case got.Port != ports[want.ModelID] || seenPorts[got.Port]:
			return nil, "", fmt.Errorf("%w: member port differs from its assignment", errRecoveryIncomplete)
		case got.WeightsVolumeID == "" || seenVolumes[got.WeightsVolumeID]:
			return nil, "", fmt.Errorf("%w: member weights volume missing or shared", errRecoveryIncomplete)
		}
		seenPorts[got.Port] = true
		seenVolumes[got.WeightsVolumeID] = true
		if want.Family == FamilyMeta {
			primary = want.ModelID
		}
		targets[want.ModelID] = readinessTarget{
			BaseURL: "http://" + net.JoinHostPort(rec.PrivateIP, strconv.Itoa(got.Port)),
			Path:    readinessPath(got.Family),
		}
	}
	if rec.Members[primary].WeightsVolumeID != rec.WeightsVolumeID {
		return nil, "", fmt.Errorf("%w: primary weights volume differs", errRecoveryIncomplete)
	}
	return targets, targets[primary].BaseURL, nil
}
