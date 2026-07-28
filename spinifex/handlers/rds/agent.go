package handlers_rds

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// Bootstrap modes returned by GetDBBootstrapConfig (D8, Q2).
const (
	// BootstrapModeInitialize is the first fetch for an instance. It carries the
	// master password and flips the one-way marker.
	BootstrapModeInitialize = "initialize"
	// BootstrapModeAttach is every fetch after: the same payload minus the
	// password, so a fresh VM booting against an existing datadir still gets its
	// port, parameters and certificate.
	BootstrapModeAttach = "attach"
)

// The agent protocol types below are internal agent↔control-plane shapes rather
// than AWS SDK types. They carry both json and locationName tags because the
// same value crosses NATS as JSON and leaves the gateway as Query-protocol XML.
// Optional fields are pointers so a nil renders as an absent XML element rather
// than an empty one — which is what keeps MasterUserPassword out of an attach
// response entirely.

// RegisterDBInstanceInput is the agent's boot-time registration. InstanceID and
// DBInstanceIdentifier are set by the gateway from the caller's resolved
// identity, never from the agent's request body.
type RegisterDBInstanceInput struct {
	DBInstanceIdentifier string `json:"dbInstanceIdentifier"`
	InstanceID           string `json:"instanceId"`
	AgentVersion         string `json:"agentVersion,omitempty"`
	EngineVersion        string `json:"engineVersion,omitempty"`
}

// RegisterDBInstanceOutput acknowledges registration and hands the agent the
// heartbeat cadence, so the interval stays control-plane-owned.
type RegisterDBInstanceOutput struct {
	DBInstanceIdentifier     string `json:"dbInstanceIdentifier" locationName:"DBInstanceIdentifier"`
	HeartbeatIntervalSeconds int64  `json:"heartbeatIntervalSeconds" locationName:"HeartbeatIntervalSeconds"`
}

// SubmitDBStateChangeInput is the periodic beat. It folds liveness into the
// state report rather than using a separate heartbeat call, so a healthy
// instance costs one round trip per tick.
type SubmitDBStateChangeInput struct {
	DBInstanceIdentifier string       `json:"dbInstanceIdentifier"`
	InstanceID           string       `json:"instanceId"`
	EngineHealth         EngineHealth `json:"engineHealth"`
	EngineVersion        string       `json:"engineVersion,omitempty"`
	Message              string       `json:"message,omitempty"`
}

// SubmitDBStateChangeOutput acknowledges the beat. Persisted reports whether it
// reached KV, which is diagnostic only — the agent's behaviour is the same
// either way.
type SubmitDBStateChangeOutput struct {
	Acknowledged             bool  `json:"acknowledged" locationName:"Acknowledged"`
	Persisted                bool  `json:"persisted" locationName:"Persisted"`
	HeartbeatIntervalSeconds int64 `json:"heartbeatIntervalSeconds" locationName:"HeartbeatIntervalSeconds"`
}

// GetDBBootstrapConfigInput is the agent's first call on every boot.
type GetDBBootstrapConfigInput struct {
	DBInstanceIdentifier string `json:"dbInstanceIdentifier"`
	InstanceID           string `json:"instanceId"`
}

// GetDBBootstrapConfigOutput is everything rds-init needs to bootstrap or
// attach. The serving cert and key are minted per call and never persisted
// (D14); MasterUserPassword is present only in initialize mode.
type GetDBBootstrapConfigOutput struct {
	Mode                 string  `json:"mode" locationName:"Mode"`
	DBInstanceIdentifier string  `json:"dbInstanceIdentifier" locationName:"DBInstanceIdentifier"`
	Engine               string  `json:"engine" locationName:"Engine"`
	EngineVersion        string  `json:"engineVersion,omitempty" locationName:"EngineVersion"`
	DBName               string  `json:"dbName,omitempty" locationName:"DBName"`
	MasterUsername       string  `json:"masterUsername" locationName:"MasterUsername"`
	MasterUserPassword   *string `json:"masterUserPassword,omitempty" locationName:"MasterUserPassword"`
	Port                 int64   `json:"port" locationName:"Port"`

	Parameters []Parameter `json:"parameters,omitempty" locationName:"Parameters" locationNameList:"member"`

	// ServingCertificate/ServingPrivateKey are empty when no cluster CA is
	// configured; the agent then starts the engine without TLS rather than
	// failing to boot, since TLS is offered and not enforced (D14).
	ServingCertificate string `json:"servingCertificate,omitempty" locationName:"ServingCertificate"`
	ServingPrivateKey  string `json:"servingPrivateKey,omitempty" locationName:"ServingPrivateKey"`
	CACertificate      string `json:"caCertificate,omitempty" locationName:"CACertificate"`
}

// Command is one directive delivered to an agent on its long poll. Concrete
// types land with their owning phases — live password apply, parameter reload,
// filesystem grow, snapshot quiesce.
type Command struct {
	CommandID  string      `json:"commandId" locationName:"CommandId"`
	Type       string      `json:"type" locationName:"Type"`
	Parameters []Parameter `json:"parameters,omitempty" locationName:"Parameters" locationNameList:"member"`
	IssuedAt   *time.Time  `json:"issuedAt,omitempty" locationName:"IssuedAt" type:"timestamp"`
}

// CommandReply is the agent's result for a command delivered on an earlier poll,
// carried on the next poll request and republished to the issuer.
type CommandReply struct {
	CommandID string `json:"commandId" locationName:"CommandId"`
	Status    string `json:"status" locationName:"Status"`
	Message   string `json:"message,omitempty" locationName:"Message"`
}

// Command reply statuses.
const (
	CommandStatusSucceeded = "succeeded"
	CommandStatusFailed    = "failed"
)

// RegisterDBInstance records the agent's boot and refreshes liveness. Idempotent:
// re-registering an already-registered instance is the normal case after an
// agent restart and simply refreshes the record.
func (s *Service) RegisterDBInstance(ctx context.Context, input *RegisterDBInstanceInput, accountID string) (*RegisterDBInstanceOutput, error) {
	if input.DBInstanceIdentifier == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	key := DBInstanceKey(input.DBInstanceIdentifier)

	var rec DBInstanceRecord
	rev, found, err := getJSONRevision(ctx, kv, key, &rec)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New(awserrors.ErrorDBInstanceNotFound)
	}

	now := time.Now().UTC()
	// RegisteredAt marks the first registration of this VM, so it is reset when a
	// replace brings a new instance ID rather than carried forward.
	if rec.Agent.InstanceID != input.InstanceID || rec.Agent.RegisteredAt == nil {
		registered := now
		rec.Agent.RegisteredAt = &registered
	}
	rec.Agent.InstanceID = input.InstanceID
	rec.Agent.AgentVersion = input.AgentVersion
	if input.EngineVersion != "" {
		rec.Agent.EngineVersion = input.EngineVersion
	}
	rec.Agent.LastSeen = &now
	rec.UpdatedAt = now

	if err := updateJSON(ctx, kv, key, rev, &rec); err != nil {
		return nil, err
	}
	// Register is a KV write by definition, so the beat counter restarts here.
	s.noteBeat(accountID, input.DBInstanceIdentifier, rec.Agent.EngineHealth, rec.Agent.Message, true)

	return &RegisterDBInstanceOutput{
		DBInstanceIdentifier:     input.DBInstanceIdentifier,
		HeartbeatIntervalSeconds: int64(HeartbeatInterval.Seconds()),
	}, nil
}

// SubmitDBStateChange records the agent's engine health. It persists on a change
// of health or message and on the slower floor, holding intermediate beats in
// memory so a steady fleet stays off the KV hot path (D13).
func (s *Service) SubmitDBStateChange(ctx context.Context, input *SubmitDBStateChangeInput, accountID string) (*SubmitDBStateChangeOutput, error) {
	if input.DBInstanceIdentifier == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if !ValidEngineHealth(input.EngineHealth) {
		return nil, fmt.Errorf("%s: unknown engine health %q", awserrors.ErrorInvalidParameterValue, input.EngineHealth)
	}

	persist := s.noteBeat(accountID, input.DBInstanceIdentifier, input.EngineHealth, input.Message, false)
	out := &SubmitDBStateChangeOutput{
		Acknowledged:             true,
		HeartbeatIntervalSeconds: int64(HeartbeatInterval.Seconds()),
	}
	if !persist {
		return out, nil
	}

	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	key := DBInstanceKey(input.DBInstanceIdentifier)

	var rec DBInstanceRecord
	rev, found, err := getJSONRevision(ctx, kv, key, &rec)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New(awserrors.ErrorDBInstanceNotFound)
	}

	now := time.Now().UTC()
	rec.Agent.InstanceID = input.InstanceID
	rec.Agent.EngineHealth = input.EngineHealth
	rec.Agent.Message = input.Message
	if input.EngineVersion != "" {
		rec.Agent.EngineVersion = input.EngineVersion
	}
	rec.Agent.LastSeen = &now
	rec.UpdatedAt = now

	if err := updateJSON(ctx, kv, key, rev, &rec); err != nil {
		return nil, err
	}
	out.Persisted = true
	return out, nil
}

// noteBeat records a beat in memory and reports whether it must reach KV.
// A changed health or message persists immediately; an unchanged one persists
// only once every HeartbeatPersistEvery beats.
func (s *Service) noteBeat(accountID, dbID string, health EngineHealth, message string, force bool) bool {
	k := accountID + "/" + dbID
	s.livenessMu.Lock()
	defer s.livenessMu.Unlock()

	live, ok := s.liveness[k]
	if !ok {
		live = &agentLiveness{}
		s.liveness[k] = live
	}
	changed := !ok || live.health != health || live.message != message

	live.lastSeen = time.Now().UTC()
	live.health = health
	live.message = message
	live.beatsSinceKV++

	if force || changed || live.beatsSinceKV >= HeartbeatPersistEvery {
		live.beatsSinceKV = 0
		return true
	}
	return false
}

// LastSeen returns the in-memory beat time for an instance, which is fresher
// than the record's persisted LastSeen. The false result means this node has
// seen no beat — after a leader change that is normal, and the caller falls back
// to the record.
func (s *Service) LastSeen(accountID, dbID string) (time.Time, bool) {
	s.livenessMu.Lock()
	defer s.livenessMu.Unlock()
	live, ok := s.liveness[accountID+"/"+dbID]
	if !ok || live.lastSeen.IsZero() {
		return time.Time{}, false
	}
	return live.lastSeen, true
}

// GetDBBootstrapConfig serves the agent's boot material and, on the first call,
// the master password.
//
// The mode is decided by the bootstrapConsumed marker, not by the action being
// one-shot: the consuming call clears the cleartext password and sets the marker
// in a single CAS, so a replay reads attach and the password is unrecoverable,
// while a fresh VM over an existing datadir still gets everything else (Q2).
func (s *Service) GetDBBootstrapConfig(ctx context.Context, input *GetDBBootstrapConfigInput, accountID string) (*GetDBBootstrapConfigOutput, error) {
	if input.DBInstanceIdentifier == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	key := DBInstanceKey(input.DBInstanceIdentifier)

	var rec DBInstanceRecord
	rev, found, err := getJSONRevision(ctx, kv, key, &rec)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New(awserrors.ErrorDBInstanceNotFound)
	}

	out := &GetDBBootstrapConfigOutput{
		Mode:                 BootstrapModeAttach,
		DBInstanceIdentifier: rec.DBInstanceIdentifier,
		Engine:               rec.Engine,
		EngineVersion:        rec.EngineVersion,
		DBName:               rec.DBName,
		MasterUsername:       rec.MasterUsername,
		Port:                 rec.Port,
		Parameters:           rec.Bootstrap.ResolvedParameters,
	}

	if !rec.Bootstrap.Consumed {
		password := rec.Bootstrap.MasterUserPassword
		now := time.Now().UTC()
		rec.Bootstrap.MasterUserPassword = ""
		rec.Bootstrap.Consumed = true
		rec.Bootstrap.ConsumedAt = &now
		rec.UpdatedAt = now
		// A lost CAS means a concurrent fetch consumed the password first. That
		// caller owns it, so this one degrades to attach rather than handing the
		// same password to two agents.
		if err := updateJSON(ctx, kv, key, rev, &rec); err != nil {
			return out, nil //nolint:nilerr // losing the race is an attach, not a failure
		}
		out.Mode = BootstrapModeInitialize
		out.MasterUserPassword = &password
	}

	cert, err := s.mintServingCert(&rec)
	if err != nil {
		return nil, err
	}
	if cert != nil {
		out.ServingCertificate = cert.CertificatePEM
		out.ServingPrivateKey = cert.PrivateKeyPEM
		out.CACertificate = cert.caPEM
	}
	return out, nil
}

// bootstrapCert bundles a minted serving cert with the CA the agent must trust.
type bootstrapCert struct {
	*ServingCert

	caPEM string
}

// mintServingCert issues a fresh serving cert for the instance. Returns nil when
// no CA is configured or the instance has no ENI address to name, since TLS is
// offered rather than enforced and neither case should block a boot.
func (s *Service) mintServingCert(rec *DBInstanceRecord) (*bootstrapCert, error) {
	caCert, caKey, err := s.loadCA()
	if err != nil {
		return nil, fmt.Errorf("rds bootstrap: load cluster CA: %w", err)
	}
	if caCert == nil || caKey == nil || rec.ENIPrivateIP == "" {
		return nil, nil
	}
	cert, err := MintServingCert(caCert, caKey, ServingCertRequest{
		DBInstanceIdentifier: rec.DBInstanceIdentifier,
		PrivateIP:            rec.ENIPrivateIP,
		DNSName:              rec.DNSName,
	})
	if err != nil {
		return nil, err
	}
	return &bootstrapCert{ServingCert: cert, caPEM: EncodeCertPEM(caCert)}, nil
}

// InstanceIndexEntry maps an internal EC2 instance ID to the DB instance it
// backs. It is the reverse lookup the agent actions resolve against, so an
// agent's system-account credentials do not require scanning every per-account
// bucket in the cluster (D3).
type InstanceIndexEntry struct {
	AccountID            string `json:"accountId"`
	DBInstanceIdentifier string `json:"dbInstanceIdentifier"`
	// VMGeneration increments on every replace, so a superseded VM's agent is
	// distinguishable from the current one.
	VMGeneration int64 `json:"vmGeneration"`
}

// LookupInstanceIndex resolves an internal instance ID to its DB instance.
// Returns (nil, nil) when the instance is not an RDS VM.
func (s *Service) LookupInstanceIndex(ctx context.Context, instanceID string) (*InstanceIndexEntry, error) {
	if instanceID == "" {
		return nil, nil
	}
	kv, err := s.systemBucket(ctx)
	if err != nil {
		return nil, err
	}
	var entry InstanceIndexEntry
	found, err := getJSON(ctx, kv, InstanceIndexKey(instanceID), &entry)
	if err != nil || !found {
		return nil, err
	}
	return &entry, nil
}

// PutInstanceIndex writes the reverse-index entry for a VM. Called at create and
// rewritten on every replace.
func (s *Service) PutInstanceIndex(ctx context.Context, instanceID string, entry InstanceIndexEntry) error {
	kv, err := s.systemBucket(ctx)
	if err != nil {
		return err
	}
	return putJSON(ctx, kv, InstanceIndexKey(instanceID), entry)
}

// DeleteInstanceIndex removes a VM's reverse-index entry at teardown or replace.
func (s *Service) DeleteInstanceIndex(ctx context.Context, instanceID string) error {
	kv, err := s.systemBucket(ctx)
	if err != nil {
		return err
	}
	return kv.Delete(ctx, InstanceIndexKey(instanceID))
}
