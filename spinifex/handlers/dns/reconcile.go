package dns

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	nsconfig "github.com/mulgadc/northstar/pkg/config"
	"github.com/mulgadc/spinifex/spinifex/config"
	reconcilelock "github.com/mulgadc/spinifex/spinifex/network/reconcile"
	"github.com/mulgadc/spinifex/spinifex/reconciler"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// DefaultReconcileInterval is how often the drift backstop rebuilds managed
// records from the live resource inventory when nothing has changed. With the
// watch carrying every change the loop sees, this is the backstop for drift
// introduced outside KV — directly in the zone object, say — rather than the
// path by which a normal lifecycle event reaches DNS.
const DefaultReconcileInterval = reconciler.DefaultResync

const (
	// maxReconcileNATSPayloadBytes stays below nats.conf's 1 MB max_payload.
	maxReconcileNATSPayloadBytes = 900 * 1024
	// reconcileNATSHeaderHeadroom reserves space for account and trace headers
	// when a deployment negotiates a lower server payload limit.
	reconcileNATSHeaderHeadroom = 4 * 1024
	changeBatchJSONOverhead     = len(`{"changes":[]}`)
)

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
}

// Reconciler converges the zone toward the live inventory. On each pass it
// rebuilds the desired managed record set: every desired record is re-UPSERTed
// (idempotent — the writer skips unchanged zones) and stale *prunable* records
// are DELETEd. A pass runs on a change to any watched resource store, and on
// the interval as the backstop for drift those stores cannot report.
//
// Each pass is gated on a CAS-elected leader so one node publishes the
// converging batch. Without it every node published its own batch, and since
// the queue group load-balances rather than serialises, an idle cluster raced
// its own zone object once per cycle.
//
// Only cluster-wide-enumerable records (load balancers, EKS clusters) are
// pruned: any node sees the full ELB/EKS set from KV. EC2 records are never
// pruned here because a node's vmMgr holds only its own instances — an
// incomplete view would delete another node's records. EC2 removal stays with
// the terminate hook; the reconcile only repairs missing/incorrect EC2 records.
type Reconciler struct {
	enabled    bool
	s3cfg      *nsconfig.S3Config
	baseDomain string
	nc         *nats.Conn
	desired    DesiredFunc
	interval   time.Duration
	accountID  string
	holder     string
	sources    []reconciler.Source
}

// NewReconciler builds the drift backstop. It is disabled (a no-op) when
// northstar S3 is not configured or no desired-set provider is supplied.
// sources name the buckets whose changes should wake the loop. They are
// optional: with none supplied the loop falls back to the interval alone,
// which is the behaviour that predates the watch.
func NewReconciler(cfg *config.Config, nc *nats.Conn, desired DesiredFunc, sources ...reconciler.Source) *Reconciler {
	r := &Reconciler{
		nc:        nc,
		desired:   desired,
		interval:  DefaultReconcileInterval,
		accountID: utils.GlobalAccountID,
		sources:   sources,
	}
	if cfg != nil {
		r.holder = cfg.Node
	}
	zoneCfg, ok := zoneS3Config(cfg)
	if !ok || desired == nil {
		return r
	}
	r.enabled = true
	r.s3cfg = zoneCfg.s3
	r.baseDomain = strings.TrimSpace(zoneCfg.server.DefaultDomain)
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
		Reconcile: func(ctx context.Context) error {
			r.reconcileOnce(ctx)
			return nil
		},
		Resync: r.interval,
	})
}

// reconcileOnce computes the converging batch and publishes it best-effort, on
// whichever node wins this cycle's election.
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
	payloadLimit := reconcilePayloadLimit(r.nc)
	if err := publishReconcileBatches(batch, payloadLimit, func(changes []Change) error {
		return publishReconcileBatch(r.nc, r.accountID, changes)
	}); err != nil {
		slog.Warn("dns reconcile: batching or publish failed, retrying next cycle", "changes", len(batch), "error", err)
		return
	}
	slog.Debug("dns reconcile: converged", "changes", len(batch))
}

// publishReconcileBatch requires the writer to acknowledge every submitted
// change, so a missing transport or partial success cannot report convergence.
func publishReconcileBatch(nc *nats.Conn, accountID string, changes []Change) error {
	res, err := PublishChanges(nc, accountID, changes)
	if err != nil {
		return err
	}
	applied := 0
	if res != nil {
		applied = res.Applied
	}
	if applied != len(changes) {
		return fmt.Errorf("writer acknowledged %d of %d changes", applied, len(changes))
	}
	return nil
}

// publishReconcileBatches sends bounded batches sequentially so each is
// acknowledged before the next begins. On failure it stops: the next cycle
// safely rebuilds and retries the complete idempotent desired state.
func publishReconcileBatches(changes []Change, payloadLimit int, publish func([]Change) error) error {
	batches, err := splitReconcileChanges(changes, payloadLimit)
	if err != nil {
		return err
	}
	for i, batch := range batches {
		if err := publish(batch); err != nil {
			return fmt.Errorf("publish batch %d of %d: %w", i+1, len(batches), err)
		}
	}
	return nil
}

// reconcilePayloadLimit respects a lower limit advertised by the connected
// server while retaining the repository policy ceiling.
func reconcilePayloadLimit(nc *nats.Conn) int {
	limit := maxReconcileNATSPayloadBytes
	if nc == nil || nc.MaxPayload() <= 0 {
		return limit
	}
	serverLimit := max(0, int(nc.MaxPayload())-reconcileNATSHeaderHeadroom)
	return min(limit, serverLimit)
}

// splitReconcileChanges preserves change order while enforcing the Route 53
// per-zone record/value request limits and the effective NATS payload ceiling.
// UPSERT costs count twice, matching Route 53 semantics.
func splitReconcileChanges(changes []Change, payloadLimit int) ([][]Change, error) {
	var batches [][]Change
	start := 0
	zoneRecords := map[string]int{}
	zoneValueChars := map[string]int{}
	currentPayloadBytes := changeBatchJSONOverhead

	flush := func(end int) {
		if start == end {
			return
		}
		batches = append(batches, changes[start:end])
		start = end
		clear(zoneRecords)
		clear(zoneValueChars)
		currentPayloadBytes = changeBatchJSONOverhead
	}

	for i, change := range changes {
		encoded, err := json.Marshal(change)
		if err != nil {
			return nil, fmt.Errorf("marshal change %d for %s: %w", i+1, change.Name, err)
		}
		multiplier := 1
		if change.Action == ActionUpsert {
			multiplier = 2
		}
		records := multiplier
		valueChars := multiplier * utf8.RuneCountInString(change.Value)
		singlePayloadBytes := changeBatchJSONOverhead + len(encoded)

		if records > MaxRecordsPerChangeRequest {
			return nil, fmt.Errorf("change %d for %s has %d record elements; maximum is %d", i+1, change.Name, records, MaxRecordsPerChangeRequest)
		}
		if valueChars > MaxValueCharsPerChangeRequest {
			return nil, fmt.Errorf("change %d for %s has %d value characters; maximum is %d", i+1, change.Name, valueChars, MaxValueCharsPerChangeRequest)
		}
		if singlePayloadBytes > payloadLimit {
			return nil, fmt.Errorf("change %d for %s serializes to %d bytes; payload maximum is %d", i+1, change.Name, singlePayloadBytes, payloadLimit)
		}

		payloadBytes := len(encoded)
		if i > start {
			payloadBytes++ // JSON comma between adjacent changes.
		}
		if i > start && (zoneRecords[change.Zone]+records > MaxRecordsPerChangeRequest ||
			zoneValueChars[change.Zone]+valueChars > MaxValueCharsPerChangeRequest ||
			currentPayloadBytes+payloadBytes > payloadLimit) {
			flush(i)
			payloadBytes = len(encoded)
		}

		zoneRecords[change.Zone] += records
		zoneValueChars[change.Zone] += valueChars
		currentPayloadBytes += payloadBytes
	}
	flush(len(changes))
	return batches, nil
}

// computeBatch reads the base zone (the only zone holding prunable ELB/EKS
// records) and converges the desired set against it.
func (r *Reconciler) computeBatch() ([]Change, error) {
	ds := r.desired()
	existing := map[string][]zoneRecord{}
	recs, ok, err := r.readZone(r.baseDomain)
	if err != nil {
		return nil, err
	}
	if ok {
		existing[r.baseDomain] = recs
	}
	return computeConverge(ds.Changes, existing, r.prunable(ds.Prunable))
}

// prunable returns the predicate deciding whether a (zone, label) record may be
// deleted when absent from the desired set: load-balancer, EKS and RDS records
// in the base domain, but only for the classes this cycle enumerated authoritatively
// across all tenants. EC2 (`.compute.`) records are never pruned (a node sees
// only its own instances); structural (apex/NS/glue) records never match.
func (r *Reconciler) prunable(scope PruneScope) func(zone, label string) bool {
	return func(zone, label string) bool {
		if zone != r.baseDomain {
			return false
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
