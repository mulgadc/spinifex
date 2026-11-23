package ochre

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
)

// errBundleIdentity marks a record whose persisted identity cannot be safely
// compared with what its co-serve group must have launched.
var errBundleIdentity = errors.New("bedrock: endpoint identity cannot be verified against its catalog group")

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
	if member, live := allMembersLive(probeCtx, r.svc.httpClient(), targets); !live {
		slog.InfoContext(ctx, "bedrock reaper: parked endpoint member not ready; left parked",
			"model", observed.ModelID, "member", member, "instanceId", observed.InstanceID)
		return
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
		slog.ErrorContext(ctx, "bedrock reaper: promoting a recovered endpoint is unconfirmed; it may have landed",
			"model", observed.ModelID, "instanceId", observed.InstanceID, "err", err)
		return
	}
	slog.InfoContext(ctx, "bedrock reaper: parked endpoint recovered to READY",
		"model", observed.ModelID, "instanceId", observed.InstanceID)
}

// allMembersLive probes every member at once within ctx, so each gets the full
// scrape budget. It names a member that did not answer a direct 200; that
// failure cancels the other probes.
func allMembersLive(ctx context.Context, client *http.Client, targets map[string]readinessTarget) (string, bool) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var mu sync.Mutex
	failed := ""

	wg.Add(len(targets))
	for modelID, target := range targets {
		go func(modelID string, target readinessTarget) {
			defer wg.Done()
			if probeLiveness(ctx, client, target.BaseURL+target.Path) == livenessLive {
				return
			}
			mu.Lock()
			if failed == "" {
				failed = modelID
			}
			mu.Unlock()
			cancel()
		}(modelID, target)
	}
	wg.Wait()
	return failed, failed == ""
}

// sameParked reports whether current is still the parked incarnation and
// generation observed, with the same reason.
func sameParked(current, observed EndpointRecord) bool {
	return current.Parked() && sameIncarnation(current, observed) &&
		current.Generation == observed.Generation && current.ParkedReason == observed.ParkedReason
}

// parkedTargets checks a parked rec's marker and its co-serve group, and
// returns each member's readiness target and the primary's base URL.
func parkedTargets(rec EndpointRecord) (map[string]readinessTarget, string, error) {
	switch rec.ParkedReason {
	case ParkReadinessRedirected, ParkReadinessTimedOut:
	default:
		return nil, "", fmt.Errorf("%w: unknown park reason", errBundleIdentity)
	}
	if rec.ParkedAt.IsZero() {
		return nil, "", fmt.Errorf("%w: missing marker or resource identity", errBundleIdentity)
	}
	bundle, err := catalogBundle(rec)
	if err != nil {
		return nil, "", err
	}
	return bundle.targets, bundle.targets[bundle.primary].BaseURL, nil
}

// bundleIdentity is a record's co-serve group as verified against the catalog:
// every member's own target, and the primary member chosen as at launch.
type bundleIdentity struct {
	targets       map[string]readinessTarget
	primary       string
	primaryFamily string
}

// catalogBundle checks rec against the co-serve group its model launches with.
// Every member must match the catalog's set, family and assigned port exactly,
// on the record's own address, with its own weights volume.
func catalogBundle(rec EndpointRecord) (bundleIdentity, error) {
	if rec.CreatedAt.IsZero() || rec.NodeID == "" || rec.InstanceID == "" || rec.ENIID == "" ||
		rec.WeightsVolumeID == "" || net.ParseIP(rec.PrivateIP) == nil {
		return bundleIdentity{}, fmt.Errorf("%w: missing marker or resource identity", errBundleIdentity)
	}
	spec, found, selfHost := LookupCoServeGroup(rec.ModelID)
	if !found || !selfHost {
		return bundleIdentity{}, fmt.Errorf("%w: model is not a self-hosted catalog entry", errBundleIdentity)
	}
	if len(rec.Members) != len(spec.Members) {
		return bundleIdentity{}, fmt.Errorf("%w: member set differs from the catalog", errBundleIdentity)
	}
	inputs := make([]LaunchMemberInput, 0, len(spec.Members))
	for _, m := range spec.Members {
		inputs = append(inputs, LaunchMemberInput{ModelID: m.ModelID, Family: m.Family})
	}
	ports := assignMemberPorts(inputs)

	bundle := bundleIdentity{
		targets:       make(map[string]readinessTarget, len(spec.Members)),
		primary:       spec.Members[0].ModelID,
		primaryFamily: spec.Members[0].Family,
	}
	seenPorts := make(map[int]bool, len(spec.Members))
	seenVolumes := make(map[string]bool, len(spec.Members))
	for _, want := range spec.Members {
		got, ok := rec.Members[want.ModelID]
		switch {
		case !ok:
			return bundleIdentity{}, fmt.Errorf("%w: member set differs from the catalog", errBundleIdentity)
		case got.Family != want.Family:
			return bundleIdentity{}, fmt.Errorf("%w: member family differs from the catalog", errBundleIdentity)
		case got.Port != ports[want.ModelID] || seenPorts[got.Port]:
			return bundleIdentity{}, fmt.Errorf("%w: member port differs from its assignment", errBundleIdentity)
		case got.WeightsVolumeID == "" || seenVolumes[got.WeightsVolumeID]:
			return bundleIdentity{}, fmt.Errorf("%w: member weights volume missing or shared", errBundleIdentity)
		}
		seenPorts[got.Port] = true
		seenVolumes[got.WeightsVolumeID] = true
		if want.Family == FamilyMeta {
			bundle.primary, bundle.primaryFamily = want.ModelID, want.Family
		}
		bundle.targets[want.ModelID] = readinessTarget{
			BaseURL: "http://" + net.JoinHostPort(rec.PrivateIP, strconv.Itoa(got.Port)),
			Path:    readinessPath(got.Family),
		}
	}
	if rec.Members[bundle.primary].WeightsVolumeID != rec.WeightsVolumeID {
		return bundleIdentity{}, fmt.Errorf("%w: primary weights volume differs", errBundleIdentity)
	}
	return bundle, nil
}
