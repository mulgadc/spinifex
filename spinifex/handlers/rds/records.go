package handlers_rds

import "time"

// Fields are grouped by writer: the AWS API owns the configuration, the
// reconciler the plumbing, the agent protocol Bootstrap and Agent.
type DBInstanceRecord struct {
	DBInstanceIdentifier string `json:"dbInstanceIdentifier"`
	AccountID            string `json:"accountId"`
	Status               Status `json:"status"`

	// AWS's immutable per-instance handle, distinct from the identifier a
	// customer may reuse. The Terraform provider keys its own state off it and
	// reads the instance back by filtering on it, so an instance without one is
	// unmanageable by Terraform. Assigned at create and never changed.
	DbiResourceID string `json:"dbiResourceId,omitempty"`

	Engine           string `json:"engine"`
	EngineVersion    string `json:"engineVersion"`
	DBInstanceClass  string `json:"dbInstanceClass"`
	AllocatedStorage int64  `json:"allocatedStorage"`
	StorageType      string `json:"storageType,omitempty"`
	DBName           string `json:"dbName,omitempty"`
	MasterUsername   string `json:"masterUsername"`
	Port             int64  `json:"port"`

	// A one-way marker. A rotated password is handed to the agent over the
	// command channel and never persisted, so this records only that it changed.
	MasterPasswordUpdatedAt *time.Time `json:"masterPasswordUpdatedAt,omitempty"`

	// Blocks DeleteDBInstance outright. Settable at create and modify.
	DeletionProtection bool `json:"deletionProtection,omitempty"`

	// Accepted inert settings are recorded so describe echoes what the client set.
	// Dropping them would leave Terraform planning changes no modify can deliver.
	AutoMinorVersionUpgrade   bool  `json:"autoMinorVersionUpgrade"`
	CopyTagsToSnapshot        bool  `json:"copyTagsToSnapshot"`
	MonitoringInterval        int64 `json:"monitoringInterval"`
	EnablePerformanceInsights bool  `json:"enablePerformanceInsights"`

	// The backup policy in force. Both windows are stored in AWS's canonical text
	// so a describe reads back the string a later modify compares against; an
	// empty one is derived from the instance identifier rather than left unset.
	BackupRetentionPeriod      int64  `json:"backupRetentionPeriod,omitempty"`
	PreferredBackupWindow      string `json:"preferredBackupWindow,omitempty"`
	PreferredMaintenanceWindow string `json:"preferredMaintenanceWindow,omitempty"`

	// When the last automated backup succeeded. Persisted rather than held in
	// leader memory, so leader churn, a daemon restart, or two nodes briefly
	// believing they hold the lease cannot fire a second backup for one window.
	LastAutomatedBackupAt *time.Time `json:"lastAutomatedBackupAt,omitempty"`

	// When the last automated backup failed or was skipped, and how many have
	// failed in a row. The stamp paces retries inside a window; the count makes a
	// persistently failing backup visible and resets on the first success. Neither
	// moves the instance out of available — a failed backup is an event, not an
	// instance failure.
	LastAutomatedBackupFailureAt *time.Time `json:"lastAutomatedBackupFailureAt,omitempty"`
	AutomatedBackupFailures      int        `json:"automatedBackupFailures,omitempty"`

	// When the maintenance window last opened a deferred modify, holding a window
	// to one apply exactly as the backup stamp does.
	LastMaintenanceWindowAt *time.Time `json:"lastMaintenanceWindowAt,omitempty"`

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
	// healthy heartbeat or an explicit lifecycle operation.
	UnhealthySince *time.Time `json:"unhealthySince,omitempty"`

	// When the lifecycle op that put the instance in its current transitional
	// state began. The reconciler bounds the transition from here and ignores
	// heartbeats older than it, so a beat from before a reboot cannot be read as
	// the reboot having finished.
	TransitionStartedAt *time.Time `json:"transitionStartedAt,omitempty"`

	// Named on the delete request and persisted before teardown starts, so a
	// resumed delete takes the same final snapshot rather than none.
	FinalSnapshotIdentifier string `json:"finalSnapshotIdentifier,omitempty"`

	// The snapshot currently being taken of this instance. Nil when none is.
	SnapshotOperation *SnapshotOperation `json:"snapshotOperation,omitempty"`

	// The DB snapshot this instance was restored from, if any. Kept so a
	// DeleteDBSnapshot the restored volume still blocks can name what to remove
	// first rather than reporting an opaque in-use fault.
	RestoredFromDBSnapshot string `json:"restoredFromDbSnapshot,omitempty"`

	// Static parameters written to the engine's config but not yet in effect.
	// Cleared by the reboot that applies them.
	PendingRebootParameters []string `json:"pendingRebootParameters,omitempty"`
	// Set when the guest rolls back a parameter set that prevented startup.
	// Cleared only after a corrected set installs successfully.
	ParametersRolledBack bool `json:"parametersRolledBack,omitempty"`

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
// crashed leader has to be able to finish.
//
// MasterUserPassword is deliberately absent: cleartext is never persisted,
// and AWS applies a password change as soon as possible regardless
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

	// The record is written creating before the EC2 snapshot is taken, so a crash
	// in between leaves a reconcilable trace rather than an orphaned EC2 snapshot.
	SnapshotStatusCreating  = "creating"
	SnapshotStatusAvailable = "available"
)

// The snapshot operation holding a DB instance, written under the same CAS that
// moves it to backing-up so a second request is rejected rather than queued. An
// An automated snapshot and a manual one serialise against each other here.
type SnapshotOperation struct {
	DBSnapshotIdentifier string `json:"dbSnapshotIdentifier"`
	// Where the instance goes when the snapshot finishes. Recorded rather than
	// assumed, because snapshotting a stopped instance must not leave it looking
	// available.
	ResumeStatus Status    `json:"resumeStatus"`
	StartedAt    time.Time `json:"startedAt"`
}

// The db-snapshots/{id} record. The EC2 snapshot holds the data; this is the
// RDS-level metadata a restore needs and DescribeDBSnapshots projects, captured
// at snapshot time because the DB instance it describes may be gone by then.
type DBSnapshotRecord struct {
	DBSnapshotIdentifier string `json:"dbSnapshotIdentifier"`
	DBInstanceIdentifier string `json:"dbInstanceIdentifier"`
	AccountID            string `json:"accountId"`
	SnapshotType         string `json:"snapshotType"`
	Status               string `json:"status"`
	// Distinguishes a delete reservation from a concurrent manual snapshot of
	// the same instance and volume. Older final snapshots are inferred while
	// their source instance remains in deleting.
	FinalSnapshot bool `json:"finalSnapshot,omitempty"`

	// The EC2 snapshot the data lives in, and the volume whose chunks it
	// references — which is why that volume is retained rather than deleted.
	SnapshotID     string `json:"snapshotId"`
	SourceVolumeID string `json:"sourceVolumeId"`

	Engine           string `json:"engine"`
	EngineVersion    string `json:"engineVersion"`
	DBInstanceClass  string `json:"dbInstanceClass,omitempty"`
	AllocatedStorage int64  `json:"allocatedStorage"`
	StorageType      string `json:"storageType,omitempty"`
	StorageEncrypted bool   `json:"storageEncrypted,omitempty"`
	DBName           string `json:"dbName,omitempty"`
	MasterUsername   string `json:"masterUsername"`
	Port             int64  `json:"port"`

	// The placement the source instance had, which a restore falls back to for
	// every field the request leaves unspecified.
	VpcID                string   `json:"vpcId,omitempty"`
	VpcSecurityGroupIDs  []string `json:"vpcSecurityGroupIds,omitempty"`
	DBSubnetGroupName    string   `json:"dbSubnetGroupName,omitempty"`
	DBParameterGroupName string   `json:"dbParameterGroupName,omitempty"`

	// Copied from the source instance so a restore keeps its rotation history: a
	// later ModifyDBInstance --master-user-password still works, and the restored
	// instance keeps the credentials the datadir was written with.
	MasterPasswordUpdatedAt *time.Time `json:"masterPasswordUpdatedAt,omitempty"`

	// True when the engine was still writing as it was taken, so a restore
	// replays WAL. A final snapshot is taken with the engine already down, so it
	// is never crash-consistent; the quiesce fallback is what sets this.
	CrashConsistent bool `json:"crashConsistent,omitempty"`

	Tags map[string]string `json:"tags,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
}

var _ TaggedRecord = (*DBSnapshotRecord)(nil)

func (r *DBSnapshotRecord) GetTags() map[string]string { return r.Tags }

func (r *DBSnapshotRecord) SetTags(tags map[string]string) { r.Tags = tags }

// The backups/{db}/automated/{ts} index entry. Deliberately thin: it exists so
// the retention sweep can enumerate one instance's automated backups without a
// bucket-wide snapshot scan, and everything else it needs — age, status, source
// volume — is read from the db-snapshots record this names.
type AutomatedBackupRecord struct {
	DBInstanceIdentifier string    `json:"dbInstanceIdentifier"`
	DBSnapshotIdentifier string    `json:"dbSnapshotIdentifier"`
	CreatedAt            time.Time `json:"createdAt"`
}

// A data volume that outlived its DB instance because a COW snapshot still
// references its chunks. The last DeleteDBSnapshot to empty Snapshots
// deletes it; the retention reaper is the backstop for a crash in between.
type RetainedVolumeRecord struct {
	VolumeID  string `json:"volumeId"`
	AccountID string `json:"accountId"`
	// The instance it belonged to, so an operator can attribute the footprint
	// after the DB instance record is gone.
	DBInstanceIdentifier string `json:"dbInstanceIdentifier"`
	// The EC2 snapshot IDs holding it alive, read from the volume store's own
	// index rather than from the RDS key space: that index is what DeleteVolume
	// enforces against, so it is the only list a release can trust.
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

// The db-subnet-groups/{name} record. The subnet list is stored verbatim rather
// than reduced to a placement, so when V2 makes AZs real the group needs no
// migration — only the code that chooses among its subnets changes.
type DBSubnetGroupRecord struct {
	Name        string `json:"name"`
	AccountID   string `json:"accountId"`
	Description string `json:"description"`
	// Every subnet the customer supplied, in request order, each with the AZ
	// recorded on the subnet itself rather than a hardcoded zone.
	Subnets []DBSubnetGroupSubnet `json:"subnets"`
	// The one VPC they all share, which is what makes the group usable for a
	// placement at all.
	VpcID string `json:"vpcId"`

	Tags map[string]string `json:"tags,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type DBSubnetGroupSubnet struct {
	SubnetID         string `json:"subnetId"`
	AvailabilityZone string `json:"availabilityZone,omitempty"`
}

var _ TaggedRecord = (*DBSubnetGroupRecord)(nil)

func (r *DBSubnetGroupRecord) GetTags() map[string]string { return r.Tags }

func (r *DBSubnetGroupRecord) SetTags(tags map[string]string) { r.Tags = tags }

// The db-parameter-groups/{name}/meta record. The values themselves live one key
// each under .../params/, so a modify touching one parameter cannot clobber a
// concurrent change to another.
type DBParameterGroupRecord struct {
	Name        string `json:"name"`
	AccountID   string `json:"accountId"`
	Family      string `json:"family"`
	Description string `json:"description"`

	Tags map[string]string `json:"tags,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

var _ TaggedRecord = (*DBParameterGroupRecord)(nil)

func (r *DBParameterGroupRecord) GetTags() map[string]string { return r.Tags }

func (r *DBParameterGroupRecord) SetTags(tags map[string]string) { r.Tags = tags }

// One stored override, at db-parameter-groups/{name}/params/{key}. ApplyMethod
// is the customer's request rather than a fact: whether a change lands live is
// decided by the parameter's own ApplyType.
type DBParameterRecord struct {
	Name        string    `json:"name"`
	Value       string    `json:"value"`
	ApplyMethod string    `json:"applyMethod,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt"`
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
