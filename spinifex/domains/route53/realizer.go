package route53

import (
	"context"
	"crypto/md5" //nolint:gosec // matches the S3 ETag northstar reports, not a security boundary
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/lifecycle/reconciler"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
)

// Publisher is where realized zones go: the northstar zone store and the
// servers that load from it.
type Publisher interface {
	PutZone(ctx context.Context, apex string, body []byte) error
	DeleteZone(ctx context.Context, apex string) error
	// Reload asks every northstar server to load apex. served holds the version
	// each answering server reports, "" for none; complete is whether every
	// server answered.
	Reload(ctx context.Context, apex string) (served []string, complete bool, err error)
}

const (
	defaultRealizeResync = time.Minute
	// realizeRetry is how soon a zone some server has not yet loaded is retried.
	realizeRetry = 5 * time.Second
)

// Realizer publishes every ACTIVE zone to northstar, withdraws every zone that
// stops being ACTIVE, and moves changes to INSYNC once every northstar server
// serves them. One node realizes at a time, through Elect.
type Realizer struct {
	store *Store
	pub   Publisher

	// Elect wins this pass for the calling node; nil means always.
	Elect func(ctx context.Context) (release func(), ok bool)
	// Reserved reports platform zone names, which are never written or deleted.
	Reserved func(name string) bool
	// Now is the clock; tests replace it.
	Now func() time.Time
	// Resync is the drift backstop; zero means one minute.
	Resync time.Duration
}

// NewRealizer returns a Realizer over store that publishes through pub.
func NewRealizer(store *Store, pub Publisher) *Realizer {
	return &Realizer{store: store, pub: pub, Now: time.Now}
}

// Run realizes once, then on every zone or record-set change.
func (r *Realizer) Run(ctx context.Context) {
	resync := r.Resync
	if resync <= 0 {
		resync = defaultRealizeResync
	}
	reconciler.Run(ctx, reconciler.Config{
		Name: "route53",
		Sources: []reconciler.Source{
			reconciler.Fixed(r.store.bucket, keyPrefixZone+">"),
			reconciler.Fixed(r.store.bucket, keyPrefixRRSets+">"),
		},
		Reconcile: r.Reconcile,
		Resync:    resync,
	})
}

// Reconcile is one whole-set pass. A zone a server has not yet loaded asks for
// a prompt revisit, since nothing in KV changes when it does.
func (r *Realizer) Reconcile(ctx context.Context) (time.Duration, error) {
	if r.Elect != nil {
		release, ok := r.Elect(ctx)
		if !ok {
			return 0, nil
		}
		defer release()
	}
	zones, err := r.store.ListAllZones(ctx)
	if err != nil {
		return 0, fmt.Errorf("list hosted zones: %w", err)
	}
	published, err := r.store.ListPublished(ctx)
	if err != nil {
		return 0, fmt.Errorf("list published zones: %w", err)
	}
	changes, err := r.store.ListAllChanges(ctx)
	if err != nil {
		return 0, fmt.Errorf("list changes: %w", err)
	}
	pending := pendingByZone(changes)

	var revisit time.Duration
	var errs []error
	served := map[string]bool{}
	for apex, zone := range r.servedZones(zones) {
		doc, _, err := r.store.GetRRSets(ctx, zone.AccountID, zone.ID)
		if errors.Is(err, kvstore.ErrNotFound) {
			continue // being deleted; withdrawn below
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("read record sets for %s: %w", apex, err))
			served[apex] = true // unknown, so leave what is published alone
			continue
		}
		served[apex] = true
		prev, had := published[apex]
		retry, err := r.realize(ctx, apex, zone, doc, prev, had, pending)
		if err != nil {
			errs = append(errs, err)
		}
		if retry || err != nil {
			revisit = realizeRetry
		}
	}
	for apex, p := range published {
		if served[apex] {
			continue
		}
		retry, err := r.withdraw(ctx, p, pending[p.AccountID+"/"+p.ZoneID])
		if err != nil {
			errs = append(errs, err)
		}
		if retry || err != nil {
			revisit = realizeRetry
		}
	}
	if err := r.pruneOrphanChanges(ctx, zones, changes); err != nil {
		errs = append(errs, err)
	}
	return revisit, errors.Join(errs...)
}

// servedZones picks, for each zone name, the ACTIVE zone northstar serves. Only
// one should be ACTIVE per name; if more are, the oldest is served, so the
// answer does not flip between them.
func (r *Realizer) servedZones(zones []ZoneRecord) map[string]*ZoneRecord {
	out := map[string]*ZoneRecord{}
	for i := range zones {
		z := &zones[i]
		if z.State != StateActive || (r.Reserved != nil && r.Reserved(z.Name)) {
			continue
		}
		apex := strings.TrimSuffix(z.Name, ".")
		if cur, ok := out[apex]; ok && (cur.CreatedAt.Before(z.CreatedAt) || (cur.CreatedAt.Equal(z.CreatedAt) && cur.ID < z.ID)) {
			continue
		}
		out[apex] = z
	}
	return out
}

// pendingByZone groups the PENDING changes by account and zone.
func pendingByZone(changes []ChangeRecord) map[string][]ChangeRecord {
	out := map[string][]ChangeRecord{}
	for _, c := range changes {
		if c.Status == ChangeStatusPending {
			out[c.AccountID+"/"+c.ZoneID] = append(out[c.AccountID+"/"+c.ZoneID], c)
		}
	}
	return out
}

// realize writes the zone if northstar does not already hold this version,
// then reloads until every server serves it.
func (r *Realizer) realize(ctx context.Context, apex string, zone *ZoneRecord, doc *RRSetsDoc, prev PublishedZone, had bool, pending map[string][]ChangeRecord) (retry bool, err error) {
	body, err := renderZone(zone, doc)
	if err != nil {
		return false, fmt.Errorf("render %s: %w", apex, err)
	}
	sum := md5.Sum(body) //nolint:gosec // see import
	digest := hex.EncodeToString(sum[:])
	waiting := pending[zone.AccountID+"/"+zone.ID]
	if had && prev.ZoneID == zone.ID && prev.Digest == digest && prev.InSync {
		// Every server already serves this content, so a change that left the
		// rendered zone unchanged is in sync as it stands.
		if err := r.store.MarkInSync(ctx, waiting, doc.Version); err != nil {
			return false, fmt.Errorf("mark changes in sync for %s: %w", apex, err)
		}
		return false, nil
	}
	next := PublishedZone{Apex: apex, AccountID: zone.AccountID, ZoneID: zone.ID, Version: doc.Version, Digest: digest}
	if !had || prev.Digest != digest || prev.ZoneID != zone.ID {
		if err := r.pub.PutZone(ctx, apex, body); err != nil {
			return false, fmt.Errorf("write zone %s: %w", apex, err)
		}
		if err := r.store.SetPublished(ctx, &next); err != nil {
			return false, fmt.Errorf("record published zone %s: %w", apex, err)
		}
		slog.Info("route53: published zone", "zone", apex, "zone_id", zone.ID, "version", doc.Version)
	}
	if !r.reloaded(ctx, apex, digest) {
		return true, nil
	}
	if err := r.store.MarkInSync(ctx, waiting, doc.Version); err != nil {
		return false, fmt.Errorf("mark changes in sync for %s: %w", apex, err)
	}
	if had && prev.ZoneID != zone.ID {
		// The name moved to another zone; the one it left is no longer served.
		if err := r.store.MarkInSync(ctx, pending[prev.AccountID+"/"+prev.ZoneID], math.MaxUint64); err != nil {
			return false, fmt.Errorf("mark changes in sync for %s: %w", apex, err)
		}
	}
	next.InSync = true
	if err := r.store.SetPublished(ctx, &next); err != nil {
		return false, fmt.Errorf("record published zone %s: %w", apex, err)
	}
	return false, nil
}

// withdraw deletes a zone northstar should no longer serve.
func (r *Realizer) withdraw(ctx context.Context, p PublishedZone, waiting []ChangeRecord) (retry bool, err error) {
	if r.Reserved != nil && r.Reserved(p.Apex+".") {
		slog.Error("route53: refusing to withdraw a platform zone", "zone", p.Apex)
		return false, nil
	}
	if err := r.pub.DeleteZone(ctx, p.Apex); err != nil {
		return false, fmt.Errorf("delete zone %s: %w", p.Apex, err)
	}
	if !r.reloaded(ctx, p.Apex, "") {
		return true, nil
	}
	if err := r.store.MarkInSync(ctx, waiting, math.MaxUint64); err != nil {
		return false, fmt.Errorf("mark changes in sync for %s: %w", p.Apex, err)
	}
	if err := r.store.DeletePublished(ctx, p.Apex); err != nil {
		return false, fmt.Errorf("forget withdrawn zone %s: %w", p.Apex, err)
	}
	slog.Info("route53: withdrew zone", "zone", p.Apex, "zone_id", p.ZoneID)
	return false, nil
}

// reloaded reports whether every northstar server now serves digest.
func (r *Realizer) reloaded(ctx context.Context, apex, digest string) bool {
	served, complete, err := r.pub.Reload(ctx, apex)
	if err != nil {
		slog.Warn("route53: zone reload failed", "zone", apex, "error", err)
		return false
	}
	if !complete {
		return false
	}
	for _, v := range served {
		if v != digest {
			return false
		}
	}
	return true
}

// pruneOrphanChanges deletes the INSYNC changes of deleted zones once they are
// past the retention GetChange honours.
func (r *Realizer) pruneOrphanChanges(ctx context.Context, zones []ZoneRecord, changes []ChangeRecord) error {
	live := make(map[string]bool, len(zones))
	for _, z := range zones {
		live[z.AccountID+"/"+z.ID] = true
	}
	cutoff := r.Now().Add(-changeRetention)
	for _, c := range changes {
		if live[c.AccountID+"/"+c.ZoneID] || c.Status != ChangeStatusInSync || !c.SubmittedAt.Before(cutoff) {
			continue
		}
		if err := r.store.DeleteChange(ctx, c.AccountID, c.ID); err != nil {
			return fmt.Errorf("delete change %s: %w", c.ID, err)
		}
	}
	return nil
}
