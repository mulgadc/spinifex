// Package dns is the control-plane DNS record writer. Every daemon consumes
// dns.recordset.change from one queue group and owns the read-modify-write of
// zone TOML files in s3://northstar/, using the system predastore credentials.
// The queue group balances load, so the read-modify-write is serialised per zone
// by an explicit lock. Northstar itself stays read-only (N4 intact).
package dns

import "time"

// NATS transport for record-set changes.
const (
	// SubjectRecordsetChange is the request-reply subject lifecycle handlers
	// publish DNS changes on.
	SubjectRecordsetChange = "dns.recordset.change"
	// SubjectZoneReload is the fan-out subject the writer publishes after a zone
	// PUT so every northstar instance reloads just that zone immediately, instead
	// of waiting for the next S3 sync poll. No queue group: all servers consume.
	SubjectZoneReload = "dns.zone.reload"
	// QueueGroup delivers each message to exactly one writer. It does NOT
	// serialise distinct messages: concurrent changes land on different daemons in
	// parallel, so the per-zone read-modify-write takes its own lock.
	QueueGroup = "spinifex-workers"
	// KVBucketDNSReconcile elects the one node that publishes a converging batch
	// per drift cycle. Its own bucket, so the election never blocks another
	// reconcile loop.
	KVBucketDNSReconcile = "spinifex-dns-reconcile"
	// PrivateZone is the fixed AWS-parity private DNS zone (IMDS synthHostname).
	PrivateZone = "compute.internal"
	// DefaultTTL is applied when a change omits a TTL.
	DefaultTTL uint32 = 60
	// requestTimeout bounds a producer's wait for the writer's ack.
	requestTimeout = 5 * time.Second
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
