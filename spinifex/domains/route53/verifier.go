package route53

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/lifecycle/reconciler"
)

const (
	defaultVerifyInterval = time.Minute
	// defaultPublicBound limits one walk from the roots, well inside the
	// gateway's 30 s wait, so an unreachable internet reads as unknown.
	defaultPublicBound = 15 * time.Second
	// A new zone is checked every pass for this long, while its owner is most
	// likely changing the registrar, and hourly after it.
	newZoneWindow = 48 * time.Hour
	slowRecheck   = time.Hour
	activeRecheck = 24 * time.Hour
	// lostAfter is how long an active zone's delegation may fail before the
	// zone stops being served.
	lostAfter = 72 * time.Hour

	reasonContested = "the domain is active in another account"
)

// Delegation is the NS set a parent zone publishes for a name.
type Delegation struct {
	Source  string
	Servers []string
}

// Verifier activates a zone once its parent delegates it to this cluster's
// name servers, and stops serving one whose delegation has moved away.
type Verifier struct {
	store *Store

	// Platform reads a delegation from a platform zone this cluster serves; ok
	// is false when the name lies under none of them.
	Platform func(ctx context.Context, name string) (d Delegation, ok bool, err error)
	// Public asks the parent's authoritative servers, without recursion.
	Public func(ctx context.Context, name string) (Delegation, error)
	// Elect wins this pass for the calling node; nil means always.
	Elect func(ctx context.Context) (release func(), ok bool)
	Now   func() time.Time
	// Interval is the pass period; zero means one minute.
	Interval time.Duration
	// PublicBound limits one Public lookup; zero means 15 s.
	PublicBound time.Duration
}

// NewVerifier returns a Verifier over store.
func NewVerifier(store *Store) *Verifier {
	return &Verifier{store: store, Now: time.Now}
}

// Run verifies on every interval. Nothing in KV changes when a registrar does,
// so the loop is driven by time alone.
func (v *Verifier) Run(ctx context.Context) {
	interval := v.Interval
	if interval <= 0 {
		interval = defaultVerifyInterval
	}
	reconciler.Run(ctx, reconciler.Config{Name: "route53-verify", Reconcile: v.Reconcile, Resync: interval})
}

// Reconcile checks every zone that is due.
func (v *Verifier) Reconcile(ctx context.Context) (time.Duration, error) {
	if v.Elect != nil {
		release, ok := v.Elect(ctx)
		if !ok {
			return 0, nil
		}
		defer release()
	}
	zones, err := v.store.ListAllZones(ctx)
	if err != nil {
		return 0, fmt.Errorf("list hosted zones: %w", err)
	}
	now := v.Now().UTC()
	holders := activeHolders(zones)
	var due []*ZoneRecord
	for i := range zones {
		z := &zones[i]
		if checkDue(z, now) || (z.Override == "" && z.State == StateActive && holders[z.Name] != zoneRef(z)) {
			due = append(due, z)
		}
	}
	return 0, v.verify(ctx, zones, due, now)
}

// activeHolders maps each name to the oldest ACTIVE zone of it, which is the
// one the realizer serves when a race has activated more than one.
func activeHolders(zones []ZoneRecord) map[string]string {
	oldest := map[string]*ZoneRecord{}
	for i := range zones {
		z := &zones[i]
		if cur, ok := oldest[z.Name]; z.State == StateActive && (!ok || olderFirst(z, cur) < 0) {
			oldest[z.Name] = z
		}
	}
	out := make(map[string]string, len(oldest))
	for name, z := range oldest {
		out[name] = zoneRef(z)
	}
	return out
}

func zoneRef(z *ZoneRecord) string { return z.AccountID + "/" + z.ID }

// Verify checks one zone now, whatever its schedule, and returns it updated.
func (v *Verifier) Verify(ctx context.Context, account, id string) (*ZoneRecord, error) {
	zones, err := v.store.ListAllZones(ctx)
	if err != nil {
		return nil, fmt.Errorf("list hosted zones: %w", err)
	}
	i := slices.IndexFunc(zones, func(z ZoneRecord) bool { return z.AccountID == account && z.ID == id })
	if i < 0 {
		return v.store.GetZone(ctx, account, id) // reports not-found the usual way
	}
	if zones[i].Override == "" {
		if err := v.verify(ctx, zones, []*ZoneRecord{&zones[i]}, v.Now().UTC()); err != nil {
			return nil, err
		}
	}
	return v.store.GetZone(ctx, account, id)
}

// checkDue reports whether a zone's next check has come round.
func checkDue(z *ZoneRecord, now time.Time) bool {
	if z.Override != "" || z.State == StateSuspended {
		return false
	}
	var every time.Duration
	switch {
	case z.State == StateActive && z.FailingSince.IsZero():
		every = activeRecheck
	case z.State == StateActive:
		every = slowRecheck
	case now.Sub(z.CreatedAt) >= newZoneWindow:
		every = slowRecheck
	}
	return now.Sub(z.CheckedAt) >= every
}

type outcome struct {
	zone   *ZoneRecord
	d      Delegation
	reason string // empty when the delegation passes
	err    error  // the check itself could not be made
}

func (v *Verifier) verify(ctx context.Context, all []ZoneRecord, due []*ZoneRecord, now time.Time) error {
	results := make([]outcome, 0, len(due))
	for _, z := range due {
		d, err := v.delegation(ctx, all, z)
		o := outcome{zone: z, d: d, err: err}
		if err == nil {
			o.reason = compareDelegation(z.Name, d, zoneNameservers(z))
		}
		results = append(results, o)
	}

	// First match wins: the earliest-created zone of a name that passes is the
	// one activated, and no zone activates while another of its name is active.
	slices.SortStableFunc(results, func(a, b outcome) int { return olderFirst(a.zone, b.zone) })
	active := activeHolders(all)

	var errs []error
	for _, o := range results {
		self := zoneRef(o.zone)
		contested := false
		if holder, ok := active[o.zone.Name]; ok && holder != self {
			contested = true
		}
		err := v.store.MutateZone(ctx, o.zone.AccountID, o.zone.ID, func(z *ZoneRecord) (bool, error) {
			if z.Override != "" || z.State == StateSuspended {
				return false, nil
			}
			applyOutcome(z, o, contested, now)
			return true, nil
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("record check of %s: %w", o.zone.Name, err))
			continue
		}
		if o.err == nil && o.reason == "" && !contested {
			if _, ok := active[o.zone.Name]; !ok {
				active[o.zone.Name] = self
				slog.Info("route53: zone activated", "zone", o.zone.Name, "zone_id", o.zone.ID, "account", o.zone.AccountID, "parent", o.d.Source)
			}
		}
	}
	return errors.Join(errs...)
}

// applyOutcome moves a zone's activation state for one check.
func applyOutcome(z *ZoneRecord, o outcome, contested bool, now time.Time) {
	z.CheckedAt = now
	if contested && z.State == StateActive {
		// A race activated a second zone of this name. The older one is served,
		// so this one waits again rather than report a state it does not have.
		z.State, z.StateReason, z.FailingSince = StatePendingNameservers, reasonContested, time.Time{}
		slog.Warn("route53: duplicate active zone returned to pending", "zone", z.Name, "zone_id", z.ID)
		return
	}
	if o.err != nil {
		// Not knowing is not failing: an unreachable parent moves nothing.
		z.StateReason = "the delegation could not be checked: " + o.err.Error()
		return
	}
	z.Delegation, z.DelegationSource = o.d.Servers, o.d.Source
	switch {
	case o.reason != "":
		z.StateReason = o.reason
		if z.State == StateActive {
			if z.FailingSince.IsZero() {
				z.FailingSince = now
			}
			if now.Sub(z.FailingSince) >= lostAfter {
				z.State = StateNameserversLost
				slog.Warn("route53: zone lost its delegation", "zone", z.Name, "zone_id", z.ID, "reason", o.reason)
			}
		}
	case z.State == StateActive:
		z.StateReason, z.FailingSince = "", time.Time{}
	case contested:
		z.StateReason = reasonContested
	default:
		z.State, z.StateReason, z.FailingSince = StateActive, "", time.Time{}
	}
}

// delegation finds the parent of z and reads what it publishes. A parent this
// cluster serves is read from its records, never through DNS.
func (v *Verifier) delegation(ctx context.Context, all []ZoneRecord, z *ZoneRecord) (Delegation, error) {
	if parent := enclosingZone(all, z); parent != nil {
		doc, _, err := v.store.GetRRSets(ctx, parent.AccountID, parent.ID)
		if err != nil {
			return Delegation{}, fmt.Errorf("read the parent zone %s: %w", parent.Name, err)
		}
		d := Delegation{Source: "hosted zone " + strings.TrimSuffix(parent.Name, ".")}
		for _, set := range doc.Sets {
			if set.Name == z.Name && set.Type == "NS" {
				d.Servers = set.Values
			}
		}
		return d, nil
	}
	if v.Platform != nil {
		d, ok, err := v.Platform(ctx, z.Name)
		if err != nil || ok {
			return d, err
		}
	}
	if v.Public == nil {
		return Delegation{}, errors.New("no public DNS lookup is configured")
	}
	bound := v.PublicBound
	if bound <= 0 {
		bound = defaultPublicBound
	}
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	return v.Public(ctx, z.Name)
}

// enclosingZone returns the most specific active zone strictly above z, from
// any account: that zone, not the public parent, decides who holds z.
func enclosingZone(all []ZoneRecord, z *ZoneRecord) *ZoneRecord {
	var best *ZoneRecord
	for i := range all {
		p := &all[i]
		if p.State != StateActive || p.Name == z.Name || !withinZone(z.Name, p.Name) {
			continue
		}
		if best == nil || len(p.Name) > len(best.Name) || (p.Name == best.Name && olderFirst(p, best) < 0) {
			best = p
		}
	}
	return best
}

// zoneNameservers is the name servers the zone was handed at creation. A change
// to the cluster's published set must not fail a delegation made to them.
func zoneNameservers(z *ZoneRecord) map[string]bool {
	ours := make(map[string]bool, len(z.Nameservers))
	for _, ns := range z.Nameservers {
		ours[normalizeName(ns)] = true
	}
	return ours
}

// compareDelegation returns why d does not delegate name to this cluster, or ""
// when it does. Every listed server must be ours: with a foreign one in the
// set, a share of resolvers would get answers we do not control.
func compareDelegation(name string, d Delegation, ours map[string]bool) string {
	if len(ours) == 0 {
		return "the zone has no delegation set"
	}
	if len(d.Servers) == 0 {
		return fmt.Sprintf("%s does not delegate %s", d.Source, strings.TrimSuffix(name, "."))
	}
	var foreign []string
	for _, s := range d.Servers {
		if n := normalizeName(s); !ours[n] {
			foreign = append(foreign, strings.TrimSuffix(n, "."))
		}
	}
	if len(foreign) > 0 {
		slices.Sort(foreign)
		return fmt.Sprintf("%s delegates %s to name servers that are not this cluster's: %s", d.Source, strings.TrimSuffix(name, "."), strings.Join(foreign, ", "))
	}
	return ""
}

func olderFirst(a, b *ZoneRecord) int {
	if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
		return c
	}
	return strings.Compare(a.ID, b.ID)
}
