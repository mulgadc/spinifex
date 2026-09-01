// Package dns is the control-plane DNS record writer. Zone TOML files in
// s3://northstar/ are written by the node that wins the reconcile election, and
// by nothing else: the desired record set is derived from the resource stores
// each pass, so there is one writer per zone and no lock is needed to serialise
// it. Northstar itself stays read-only (N4 intact).
package dns

// NATS transport for zone changes.
const (
	// SubjectZoneReload is the fan-out subject the writer publishes after a zone
	// PUT so every northstar instance reloads just that zone immediately, instead
	// of waiting for the next S3 sync poll. No queue group: all servers consume.
	SubjectZoneReload = "dns.zone.reload"
	// KVBucketDNSReconcile elects the one node that writes the zones for a drift
	// cycle. Its own bucket, so the election never blocks another reconcile loop.
	KVBucketDNSReconcile = "spinifex-dns-reconcile"
	// PrivateZone is the fixed AWS-parity private DNS zone (IMDS synthHostname).
	PrivateZone = "compute.internal"
	// DefaultTTL is applied when a change omits a TTL.
	DefaultTTL uint32 = 60
)

// Action is the change verb (UPSERT replaces the RRset, DELETE withdraws it).
type Action string

const (
	ActionUpsert Action = "upsert"
	ActionDelete Action = "delete"
)

// Change is one record-set mutation. Name is the fully-qualified record name;
// Zone is its apex (the TOML object key, minus ".toml").
type Change struct {
	Action Action `json:"action"`
	Zone   string `json:"zone"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Value  string `json:"value"`
	TTL    uint32 `json:"ttl,omitempty"`
}

// ChangeBatch groups the changes for one resource operation into a single
// request so a launch/terminate is one round-trip.
type ChangeBatch struct {
	Changes []Change `json:"changes"`
}

// ChangeResult acknowledges how many changes were applied and to which zones.
type ChangeResult struct {
	Applied int      `json:"applied"`
	Zones   []string `json:"zones,omitempty"`
}

// ZoneReload is the fan-out event published on SubjectZoneReload after a zone is
// written, telling northstar servers to reload just that zone from S3.
type ZoneReload struct {
	Zone string `json:"zone"`
}
