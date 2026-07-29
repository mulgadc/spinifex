package handlers_rds

import "time"

// Fields are grouped by writer: the AWS API owns the configuration, the
// reconciler the plumbing, the agent protocol Bootstrap and Agent.
type DBInstanceRecord struct {
	DBInstanceIdentifier string `json:"dbInstanceIdentifier"`
	AccountID            string `json:"accountId"`
	Status               Status `json:"status"`

	Engine           string `json:"engine"`
	EngineVersion    string `json:"engineVersion"`
	DBInstanceClass  string `json:"dbInstanceClass"`
	AllocatedStorage int64  `json:"allocatedStorage"`
	StorageType      string `json:"storageType,omitempty"`
	DBName           string `json:"dbName,omitempty"`
	MasterUsername   string `json:"masterUsername"`
	Port             int64  `json:"port"`

	// A one-way marker (D8). A rotated password is handed to the agent over the
	// command channel and never persisted, so this records only that it changed.
	MasterPasswordUpdatedAt *time.Time `json:"masterPasswordUpdatedAt,omitempty"`

	// Blocks DeleteDBInstance outright (D18). Settable at create and modify.
	DeletionProtection bool `json:"deletionProtection,omitempty"`

	// Stored by ModifyDBInstance but not yet acted on: rds-9's backup and
	// maintenance machinery is what consumes them.
	BackupRetentionPeriod      int64  `json:"backupRetentionPeriod,omitempty"`
	PreferredBackupWindow      string `json:"preferredBackupWindow,omitempty"`
	PreferredMaintenanceWindow string `json:"preferredMaintenanceWindow,omitempty"`

	// What a modify asked for and has not yet delivered. Nil once everything is
	// in effect, which is also what tells the reconciler a modify is finished.
	PendingModifiedValues *PendingModifiedValues `json:"pendingModifiedValues,omitempty"`

	// Held by whichever worker is applying those values, so a second one does
	// not enter the same change. Nil when nothing is being applied.
	ModifyLease *ModifyLease `json:"modifyLease,omitempty"`

	// Where the customer ENI was placed, so a replace lands the new VM's ENI in
	// the same subnet and security groups without re-deriving them.
	SubnetID             string   `json:"subnetId,omitempty"`
	VpcID                string   `json:"vpcId,omitempty"`
	VpcSecurityGroupIDs  []string `json:"vpcSecurityGroupIds,omitempty"`
	DBSubnetGroupName    string   `json:"dbSubnetGroupName,omitempty"`
	DBParameterGroupName string   `json:"dbParameterGroupName,omitempty"`

	// Changes on every replace, which is why the bus subject keys off the DB
	// instance identifier instead.
	InstanceID string `json:"instanceId,omitempty"`
	// Increments on every replace, so a superseded VM's agent is
	// distinguishable from the current one.
	VMGeneration int64  `json:"vmGeneration,omitempty"`
	DataVolumeID string `json:"dataVolumeId,omitempty"`
	ENIID        string `json:"eniId,omitempty"`
	// Disposable: a replace mints a new one, unlike the customer ENI.
	SystemENIID string `json:"systemEniId,omitempty"`
	// Stable across VM replace, so it serves as both the fallback endpoint and
	// a durable IP SAN on the serving cert.
	ENIPrivateIP    string `json:"eniPrivateIp,omitempty"`
	EndpointAddress string `json:"endpointAddress,omitempty"`
	// The vanity hostname when northstar is configured. Kept apart from
	// EndpointAddress because the cert needs it as a DNS SAN either way.
	DNSName string `json:"dnsName,omitempty"`

	// Reported from the data volume's own state rather than echoed from the
	// request, the way EC2 derives a volume's Encrypted.
	StorageEncrypted bool `json:"storageEncrypted,omitempty"`

	// Why the instance is failed. Cleared when it leaves the failed state, so a
	// stale reason cannot outlive the failure it describes.
	FailureReason string `json:"failureReason,omitempty"`

	// When the classifier first observed the instance dark, and the timestamp the
	// failure grace window is measured from. Persisted rather than held in leader
	// memory so a leader change does not restart the clock. Cleared only by a
	// healthy heartbeat or an explicit lifecycle op (D13).
	UnhealthySince *time.Time `json:"unhealthySince,omitempty"`

	// When the lifecycle op that put the instance in its current transitional
	// state began. The reconciler bounds the transition from here and ignores
	// heartbeats older than it, so a beat from before a reboot cannot be read as
	// the reboot having finished.
	TransitionStartedAt *time.Time `json:"transitionStartedAt,omitempty"`

	// Named on the delete request and persisted before teardown starts, so a
	// resumed delete takes the same final snapshot rather than none.
	FinalSnapshotIdentifier string `json:"finalSnapshotIdentifier,omitempty"`

	// Static parameters written to the engine's config but not yet in effect.
	// Cleared by the reboot that applies them (D16).
	PendingRebootParameters []string `json:"pendingRebootParameters,omitempty"`

	// Inline rather than a separate key space, so the record delete that ends the
	// instance also ends its tags.
	Tags map[string]string `json:"tags,omitempty"`

	Bootstrap BootstrapState `json:"bootstrap"`
	Agent     AgentState     `json:"agent"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// The disruptive half of a modify: every field here takes the engine down, so
// each is recorded before the work starts and cleared as it lands. That makes
// one structure serve both meanings AWS gives PendingModifiedValues — a
// deferred change waiting for its maintenance window, and an in-flight change a
// crashed leader has to be able to finish (D12/D15/D16).
//
// MasterUserPassword is deliberately absent: D8 forbids persisting the
// cleartext, and AWS applies a password change as soon as possible regardless
// of ApplyImmediately, so there is nothing to defer.
type PendingModifiedValues struct {
	AllocatedStorage     *int64 `json:"allocatedStorage,omitempty"`
	DBInstanceClass      string `json:"dbInstanceClass,omitempty"`
	DBParameterGroupName string `json:"dbParameterGroupName,omitempty"`

	// The data volume is already at its new size but the in-guest filesystem is
	// not yet on it, so a resumed grow extends the filesystem rather than
	// re-running a volume modify that would then be rejected as a shrink.
	FilesystemGrowPending bool `json:"filesystemGrowPending,omitempty"`

	// When the modify was accepted, so an operator can see how long a deferred
	// change has been waiting on its window.
	RequestedAt time.Time `json:"requestedAt"`
}

// Reports whether anything is still outstanding, which is what lets the record
// drop the whole structure rather than keep an empty one.
func (p *PendingModifiedValues) empty() bool {
	return p == nil || (p.AllocatedStorage == nil && p.DBInstanceClass == "" &&
		p.DBParameterGroupName == "" && !p.FilesystemGrowPending)
}

// An instance that has never been modified carries no pending values at all, so
// the nil case has to read as "nothing outstanding" rather than panic.
func (p *PendingModifiedValues) growingFilesystem() bool {
	return p != nil && p.FilesystemGrowPending
}

// Claimed for as long as a worker is applying PendingModifiedValues, and
// renewed while it works. A modify still inside its own API call and one a dead
// leader abandoned are the same record otherwise — both are modifying with
// values not yet applied — so this is the only thing that tells them apart.
type ModifyLease struct {
	// The node and the claim, so two workers on one node are still distinct.
	Holder    string    `json:"holder"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Whether someone is still renewing it. An expired lease is what makes an
// abandoned modify resumable rather than stuck behind a worker that is gone.
func (l *ModifyLease) live() bool {
	return l != nil && time.Now().UTC().Before(l.ExpiresAt)
}

var _ TaggedRecord = (*DBInstanceRecord)(nil)

func (r *DBInstanceRecord) GetTags() map[string]string { return r.Tags }

func (r *DBInstanceRecord) SetTags(tags map[string]string) { r.Tags = tags }

// The Consumed marker scopes the master password to a single fetch, not the
// action: replace, recovery and restore all re-fetch without one.
type BootstrapState struct {
	// Cleared by the same CAS that sets Consumed, so the cleartext outlives
	// only the boot it serves.
	MasterUserPassword string `json:"masterUserPassword,omitempty"`
	// Only ever flips forward.
	Consumed   bool       `json:"consumed"`
	ConsumedAt *time.Time `json:"consumedAt,omitempty"`
	// Already evaluated against the instance class, so the agent receives
	// literals and never a formula.
	ResolvedParameters []Parameter `json:"resolvedParameters,omitempty"`
}

// Snapshot types and statuses, matching AWS. A final snapshot is manual: the
// customer named it and only the customer removes it.
const (
	SnapshotTypeManual    = "manual"
	SnapshotTypeAutomated = "automated"

	SnapshotStatusAvailable = "available"
)

// The db-snapshots/{id} record. The EC2 snapshot holds the data; this is the
// RDS-level metadata a restore needs and DescribeDBSnapshots projects, captured
// at snapshot time because the DB instance it describes may be gone by then.
type DBSnapshotRecord struct {
	DBSnapshotIdentifier string `json:"dbSnapshotIdentifier"`
	DBInstanceIdentifier string `json:"dbInstanceIdentifier"`
	AccountID            string `json:"accountId"`
	SnapshotType         string `json:"snapshotType"`
	Status               string `json:"status"`

	// The EC2 snapshot the data lives in, and the volume whose chunks it
	// references — which is why that volume is retained rather than deleted.
	SnapshotID     string `json:"snapshotId"`
	SourceVolumeID string `json:"sourceVolumeId"`

	Engine           string `json:"engine"`
	EngineVersion    string `json:"engineVersion"`
	AllocatedStorage int64  `json:"allocatedStorage"`
	StorageType      string `json:"storageType,omitempty"`
	StorageEncrypted bool   `json:"storageEncrypted,omitempty"`
	MasterUsername   string `json:"masterUsername"`
	Port             int64  `json:"port"`
	VpcID            string `json:"vpcId,omitempty"`

	// True when the engine was still writing as it was taken, so a restore
	// replays WAL. A final snapshot is taken with the engine already down, so it
	// is never crash-consistent; rds-8's quiesce fallback is what sets this.
	CrashConsistent bool `json:"crashConsistent,omitempty"`

	Tags map[string]string `json:"tags,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
}

// A data volume that outlived its DB instance because a COW snapshot still
// references its chunks (D10). The last DeleteDBSnapshot to empty Snapshots
// deletes it; rds-9's reaper is the backstop for a crash in between.
type RetainedVolumeRecord struct {
	VolumeID  string `json:"volumeId"`
	AccountID string `json:"accountId"`
	// The instance it belonged to, so an operator can attribute the footprint
	// after the DB instance record is gone.
	DBInstanceIdentifier string `json:"dbInstanceIdentifier"`
	// The DB snapshot identifiers holding it alive.
	Snapshots []string `json:"snapshots"`
	// Set when the volume store refused the delete without naming a holder, so a
	// release must re-check rather than read the empty list as "nothing holds it".
	HoldersUnresolved bool      `json:"holdersUnresolved,omitempty"`
	RetainedAt        time.Time `json:"retainedAt"`
}

// A member list rather than a map because the XML marshaller renders a map as an
// AWS-foreign <entry> shape in nondeterministic order.
type Parameter struct {
	Name  string `json:"name" locationName:"Name"`
	Value string `json:"value" locationName:"Value"`
}

// Separate from Status: the reconciler needs both to tell "stopped because we
// stopped it" from "stopped because it died".
type EngineHealth string

const (
	// Covers initdb, crash recovery and parameter-apply restarts.
	EngineHealthStarting EngineHealth = "starting"
	EngineHealthHealthy  EngineHealth = "healthy"
	// Running but not serving.
	EngineHealthUnhealthy EngineHealth = "unhealthy"
	// Deliberately down, e.g. quiesced for a snapshot or a storage grow.
	EngineHealthStopped EngineHealth = "stopped"
)

// Rejects unrecognised values at the boundary so a newer agent cannot persist a
// health the reconciler will fail to classify.
func ValidEngineHealth(h EngineHealth) bool {
	switch h {
	case EngineHealthStarting, EngineHealthHealthy, EngineHealthUnhealthy, EngineHealthStopped:
		return true
	default:
		return false
	}
}

// Written only by RegisterDBInstance and SubmitDBStateChange.
type AgentState struct {
	// A report from an instance other than the record's current one is a
	// superseded VM still running after a replace.
	InstanceID    string       `json:"instanceId,omitempty"`
	AgentVersion  string       `json:"agentVersion,omitempty"`
	EngineVersion string       `json:"engineVersion,omitempty"`
	EngineHealth  EngineHealth `json:"engineHealth,omitempty"`
	Message       string       `json:"message,omitempty"`
	RegisteredAt  *time.Time   `json:"registeredAt,omitempty"`
	// The last *persisted* beat. Beats in between are held in leader memory, so
	// this trails the truth by up to the persist floor.
	LastSeen *time.Time `json:"lastSeen,omitempty"`
}

const (
	// Returned to the agent on register and on every state change, so the
	// cadence is control-plane-owned, not baked into the AMI.
	HeartbeatInterval = 30 * time.Second
	// Three missed ticks.
	HeartbeatStaleAfter = 3 * HeartbeatInterval
	// The floor at which an unchanged beat reaches KV: ~7 KV ops/sec of
	// liveness at 200 instances rather than 40.
	HeartbeatPersistEvery = 5
	// How far behind a live agent the record's LastSeen can be while nothing is
	// wrong. Beats are queue-group delivered, so a node that is not handling an
	// instance's beats only ever sees them through KV.
	HeartbeatPersistFloor = HeartbeatPersistEvery * HeartbeatInterval
)
