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
	DBName           string `json:"dbName,omitempty"`
	MasterUsername   string `json:"masterUsername"`
	Port             int64  `json:"port"`

	// Changes on every replace, which is why the bus subject keys off the DB
	// instance identifier instead.
	InstanceID   string `json:"instanceId,omitempty"`
	DataVolumeID string `json:"dataVolumeId,omitempty"`
	ENIID        string `json:"eniId,omitempty"`
	// Stable across VM replace, so it serves as both the fallback endpoint and
	// a durable IP SAN on the serving cert.
	ENIPrivateIP    string `json:"eniPrivateIp,omitempty"`
	EndpointAddress string `json:"endpointAddress,omitempty"`
	// The vanity hostname when northstar is configured. Kept apart from
	// EndpointAddress because the cert needs it as a DNS SAN either way.
	DNSName string `json:"dnsName,omitempty"`

	Bootstrap BootstrapState `json:"bootstrap"`
	Agent     AgentState     `json:"agent"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

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
)
