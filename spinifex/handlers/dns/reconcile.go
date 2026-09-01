package dns

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	nsconfig "github.com/mulgadc/northstar/pkg/config"
	"github.com/mulgadc/spinifex/spinifex/config"
	reconcilelock "github.com/mulgadc/spinifex/spinifex/network/reconcile"
	"github.com/mulgadc/spinifex/spinifex/reconciler"
	"github.com/nats-io/nats.go"
)

// DefaultReconcileInterval is how often the drift backstop rebuilds managed
// records from the live resource inventory when nothing has changed. With the
// watch carrying every change the loop sees, this is the backstop for drift
// introduced outside KV — directly in the zone object, say — rather than the
// path by which a normal lifecycle event reaches DNS.
const DefaultReconcileInterval = reconciler.DefaultResync

// DesiredFunc returns the full desired managed record set built from the live
// resource inventory across all tenants. The daemon supplies it by enumerating
// instances, load balancers, and EKS clusters.
type DesiredFunc func() DesiredSet

// DesiredSet is one cycle's view of the world: every desired managed record
// (all UPSERTs) plus the authority to prune each record class.
type DesiredSet struct {
	Changes  []Change
	Prunable PruneScope
}

// PruneScope records which prunable record classes were enumerated
// authoritatively and completely across *all tenants* this cycle. A class is
// pruned only when its flag is true, so a transient KV/store error that yields a
// partial (or empty) view can never delete another tenant's live records — the
// destructive side of the reconcile stays gated on a whole-cluster, all-tenant
// enumeration. Multi-tenancy makes this mandatory: load balancers and EKS
// clusters from every account share the base zone, so pruning on an incomplete
// account view would sync only one side of the equation.
type PruneScope struct {
	ELB bool
	EKS bool
	RDS bool

	// EC2 is set only when the instance view spanned every node. A node's own
	// VM manager does not qualify: pruning against it would delete the records
	// of every instance running elsewhere.
	EC2 bool
}

// Reconciler converges the zone toward the live inventory. On each pass it
// rebuilds the desired managed record set: every desired record is re-UPSERTed
// (idempotent — the writer skips unchanged zones) and stale *prunable* records
// are DELETEd. A pass runs on a change to any watched resource store, and on
// the interval as the backstop for drift those stores cannot report.
//
// Each pass is gated on a CAS-elected leader, which then writes the zone itself
// rather than publishing the batch to the queue group. Handing its own work to
// a peer put a second writer on the object and left the read that computed the
// deletes in a different process from the write that applied them.
//
// Only cluster-wide-enumerable records are pruned, and only for the classes a
// cycle enumerated in full. Every class the desired set carries is readable
// from KV across the whole cluster, so a pass that reads them all can prune
// them all; one that could not read a class repairs it without pruning it.
type Reconciler struct {
	enabled    bool
	s3cfg      *nsconfig.S3Config
	baseDomain string
	// internalDomain is the private zone instance records land in. It is read
	// as well as the base domain because pruning one zone and not the other
	// leaves every stale private record behind.
	internalDomain string
	nc             *nats.Conn
	// writer applies the converged batch. The reconcile owns the zone object on
	// the node that won the election, so the batch never leaves this process.
	writer   *Writer
	desired  DesiredFunc
	interval time.Duration
	holder   string
	sources  []reconciler.Source
}

// NewReconciler builds the drift backstop. It is disabled (a no-op) when
// northstar S3 is not configured, no desired-set provider is supplied, or the
// writer that applies its batches is unavailable. sources name the buckets whose
// changes should wake the loop. They are optional: with none supplied the loop
// falls back to the interval alone, which is the behaviour that predates the
// watch.
func NewReconciler(cfg *config.Config, nc *nats.Conn, writer *Writer, desired DesiredFunc, sources ...reconciler.Source) *Reconciler {
	r := &Reconciler{
		nc:       nc,
		writer:   writer,
		desired:  desired,
		interval: DefaultReconcileInterval,
		sources:  sources,
	}
	if cfg != nil {
		r.holder = cfg.Node
	}
	zoneCfg, ok := zoneS3Config(cfg)
	if !ok || desired == nil || !writer.Enabled() {
		return r
	}
	r.enabled = true
	r.s3cfg = zoneCfg.s3
	r.baseDomain = strings.TrimSpace(zoneCfg.server.DefaultDomain)
	r.internalDomain = ResolveInternalDomain(cfg)
	return r
}

// Enabled reports whether the reconcile loop will run.
func (r *Reconciler) Enabled() bool { return r.enabled }

// Run converges once, then on every change to the resource stores the desired
// set is built from, with the interval as the drift backstop. It is a no-op
// when disabled, so the daemon can start it unconditionally.
//
// Only the trigger is event-driven: each pass still rebuilds the whole desired
// set, so the watch needs to report only that something changed, not what.
func (r *Reconciler) Run(ctx context.Context) {
	if !r.enabled {
		return
	}
	reconciler.Run(ctx, reconciler.Config{
		Name:    "dns",
		Sources: r.sources,
		// No revisit deadline: the desired set is a function of the buckets
		// being watched, so a change is the only reason to run early.
		Reconcile: func(ctx context.Context) (time.Duration, error) {
			r.reconcileOnce(ctx)
			return 0, nil
		},
		Resync: r.interval,
	})
}

// reconcileOnce computes the converging batch and applies it, on whichever node
// wins this cycle's election. The read that finds the stale records and the
// write that removes them now happen in one process, so a record re-added
// between them is no longer deleted by a batch that predates it.
func (r *Reconciler) reconcileOnce(ctx context.Context) {
	if !r.enabled {
		return
	}
	if r.nc != nil {
		release, elected := reconcilelock.AcquireLeader(ctx, r.nc, KVBucketDNSReconcile, r.holder)
		if !elected {
			return
		}
		defer release()
	}
	batch, err := r.computeBatch()
	if err != nil {
		// A corrupt zone is handled in readZone; anything reaching here left the
		// desired state unapplied, so surface it rather than burying it in WARN.
		slog.Error("dns reconcile: compute batch failed, retrying next cycle", "error", err)
		return
	}
	if len(batch) == 0 {
		return
	}
	slog.Debug("dns reconcile: converging", "changes", len(batch))
	res, err := r.writer.ApplyBatch(&ChangeBatch{Changes: batch})
	if err != nil {
		slog.Warn("dns reconcile: apply failed, retrying next cycle", "changes", len(batch), "error", err)
		return
	}
	slog.Debug("dns reconcile: converged", "changes", res.Applied, "zones", res.Zones)
}

// computeBatch reads every zone holding prunable records and converges the
// desired set against them. That is the base domain, plus the private zone once
// instance records are prunable — a stale private record is as wrong as a stale
// public one, and only this zone holds it.
func (r *Reconciler) computeBatch() ([]Change, error) {
	ds := r.desired()
	existing := map[string][]zoneRecord{}
	zones := []string{r.baseDomain}
	if ds.Prunable.EC2 {
		zones = append(zones, privateZoneOrDefault(r.internalDomain))
	}
	for _, zone := range zones {
		if _, seen := existing[zone]; seen || zone == "" {
			continue
		}
		recs, ok, err := r.readZone(zone)
		if err != nil {
			return nil, err
		}
		if ok {
			existing[zone] = recs
		}
	}
	return computeConverge(ds.Changes, existing, r.prunable(ds.Prunable))
}

// prunable returns the predicate deciding whether a (zone, label) record may be
// deleted when absent from the desired set, for the classes this cycle
// enumerated authoritatively across all tenants. Structural (apex/NS/glue)
// records carry none of these prefixes, so they never match.
func (r *Reconciler) prunable(scope PruneScope) func(zone, label string) bool {
	private := privateZoneOrDefault(r.internalDomain)
	return func(zone, label string) bool {
		// The private zone holds instance records and nothing else: every other
		// producer writes the base domain.
		if zone == private {
			return scope.EC2 && strings.HasPrefix(label, ec2PrivateLabelPrefix)
		}
		if zone != r.baseDomain {
			return false
		}
		if scope.EC2 && strings.HasPrefix(label, ec2PublicLabelPrefix) {
			return true
		}
		if scope.ELB && strings.Contains(label, ".elb.") {
			return true
		}
		if scope.EKS && strings.Contains(label, ".eks.") {
			return true
		}
		if scope.RDS && strings.Contains(label, ".rds.") {
			return true
		}
		return false
	}
}

// zoneRecord is one existing A record in a zone, in (label, type, value) form.
type zoneRecord struct {
	label string
	rtype uint16
	value string
}

// readZone fetches a zone's current A records from S3. ok is false when the zone
// object does not exist yet (nothing to prune against).
func (r *Reconciler) readZone(zone string) ([]zoneRecord, bool, error) {
	cfg, exists, err := nsconfig.ReadZoneRaw(r.s3cfg, zone)
	switch {
	case isCorruptZone(err):
		// Report the zone as absent so this cycle still publishes the full desired
		// set and the writer rebuilds the object. Pruning is suppressed for free:
		// no existing records were read, so nothing can look stale.
		slog.Error("dns reconcile: zone object corrupt, rebuilding from desired state",
			"zone", zone, "error", err)
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	if !exists {
		return nil, false, nil
	}
	out := make([]zoneRecord, 0, len(cfg.Records))
	for _, rec := range cfg.Records {
		out = append(out, zoneRecord{label: rec.Domain, rtype: rec.Type, value: rec.Address})
	}
	return out, true, nil
}

// computeConverge returns the change batch that makes each zone's existing
// records match `desired`: every desired change (all UPSERTs) passes through,
// and each prunable existing RRset absent from the desired set is DELETEd.
func computeConverge(desired []Change, existing map[string][]zoneRecord, prunable func(zone, label string) bool) ([]Change, error) {
	out := make([]Change, 0, len(desired))
	out = append(out, desired...)

	want := map[string]bool{}
	for _, c := range desired {
		rtype, err := recordType(c.Type)
		if err != nil {
			return nil, fmt.Errorf("validate desired record %s: %w", c.Name, err)
		}
		want[rrKey(c.Zone, relativeLabel(c.Name, c.Zone), rtype)] = true
	}

	for zone, recs := range existing {
		for _, rec := range recs {
			if !prunable(zone, rec.label) {
				continue
			}
			if want[rrKey(zone, rec.label, rec.rtype)] {
				continue
			}
			out = append(out, Change{
				Action: ActionDelete,
				Zone:   zone,
				Name:   labelToFQDN(rec.label, zone),
				Type:   typeString(rec.rtype),
				Value:  rec.value,
			})
		}
	}
	return out, nil
}

// rrKey identifies an RRset by zone, relative label, and record type.
func rrKey(zone, label string, rtype uint16) string {
	return zone + "\x00" + strings.ToLower(label) + "\x00" + typeString(rtype)
}

// labelToFQDN reconstructs a fully-qualified name from a zone-relative label
// (the inverse of relativeLabel for non-apex records).
func labelToFQDN(label, zone string) string {
	l := strings.TrimSuffix(label, ".")
	z := strings.TrimSuffix(zone, ".")
	if l == "" {
		return z
	}
	return l + "." + z
}

// typeString maps a numeric DNS type back to its textual form (inverse of
// recordType). Only the types the writer emits are handled; others map to "A".
func typeString(rtype uint16) string {
	switch rtype {
	case nsconfig.TypeNS:
		return "NS"
	case nsconfig.TypeTXT:
		return "TXT"
	default:
		return "A"
	}
}
