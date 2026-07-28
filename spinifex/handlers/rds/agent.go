package handlers_rds

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// Bootstrap modes returned by GetDBBootstrapConfig.
const (
	// BootstrapModeInitialize is the first fetch for an instance. It carries the
	// master password and flips the one-way marker.
	BootstrapModeInitialize = "initialize"
	// BootstrapModeAttach is every fetch after: the same payload minus the
	// password, so a fresh VM booting against an existing datadir still gets its
	// port, parameters and certificate.
	BootstrapModeAttach = "attach"
)

// The agent protocol types below are internal agent↔control-plane shapes. They
// carry both json and locationName tags because the same value crosses NATS as
// JSON and leaves the gateway as Query-protocol XML. Optional fields are
// pointers so a nil renders as an absent element, keeping MasterUserPassword
// out of an attach response entirely.
//
// The agent decodes that XML with encoding/xml, which matches on the Go field
// name, so an xml tag appears only where the wire name differs.

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
// attach. The serving cert and key are minted per call and never persisted;
// MasterUserPassword is present only in initialize mode.
type GetDBBootstrapConfigOutput struct {
	Mode                 string  `json:"mode" locationName:"Mode"`
	DBInstanceIdentifier string  `json:"dbInstanceIdentifier" locationName:"DBInstanceIdentifier"`
	Engine               string  `json:"engine" locationName:"Engine"`
	EngineVersion        string  `json:"engineVersion,omitempty" locationName:"EngineVersion"`
	DBName               string  `json:"dbName,omitempty" locationName:"DBName"`
	MasterUsername       string  `json:"masterUsername" locationName:"MasterUsername"`
	MasterUserPassword   *string `json:"masterUserPassword,omitempty" locationName:"MasterUserPassword"`
	Port                 int64   `json:"port" locationName:"Port"`

	Parameters []Parameter `json:"parameters,omitempty" locationName:"Parameters" locationNameList:"member" xml:"Parameters>member"`

	// ServingCertificate/ServingPrivateKey are empty when no cluster CA is
	// configured; the agent then starts the engine without TLS rather than
	// failing to boot, since TLS is offered and not enforced.
	ServingCertificate string `json:"servingCertificate,omitempty" locationName:"ServingCertificate"`
	ServingPrivateKey  string `json:"servingPrivateKey,omitempty" locationName:"ServingPrivateKey"`
	CACertificate      string `json:"caCertificate,omitempty" locationName:"CACertificate"`
}

// Command is one directive delivered to an agent on its long poll. Concrete
// types land with their owning phases — live password apply, parameter reload,
// filesystem grow, snapshot quiesce.
type Command struct {
	CommandID  string      `json:"commandId" locationName:"CommandId" xml:"CommandId"`
	Type       string      `json:"type" locationName:"Type"`
	Parameters []Parameter `json:"parameters,omitempty" locationName:"Parameters" locationNameList:"member" xml:"Parameters>member"`
	IssuedAt   *time.Time  `json:"issuedAt,omitempty" locationName:"IssuedAt" type:"timestamp"`
}

// CommandReply is the agent's result for a command delivered on an earlier poll,
// carried on the next poll request and republished to the issuer.
type CommandReply struct {
	CommandID string `json:"commandId" locationName:"CommandId" xml:"CommandId"`
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
// memory so a steady fleet stays off the KV hot path.
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

// LastSeen returns the in-memory beat time for an instance, fresher than the
// record's persisted LastSeen. A false result means this node has seen no beat,
// which is normal after a leader change; the caller falls back to the record.
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
// the master password. The consuming call clears the cleartext and sets the
// marker in a single CAS, so a replay reads attach and gets everything else.
func (s *Service) GetDBBootstrapConfig(ctx context.Context, input *GetDBBootstrapConfigInput, accountID string) (*GetDBBootstrapConfigOutput, error) {
	if input.DBInstanceIdentifier == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	key := DBInstanceKey(input.DBInstanceIdentifier)

	for {
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

		// The cert is minted before the password is consumed, so a mint failure
		// leaves the password in KV for the agent's retry. Consuming first would
		// leave an instance that can never learn its master password.
		cert, err := s.mintServingCert(&rec)
		if err != nil {
			return nil, err
		}
		if cert != nil {
			out.ServingCertificate = cert.CertificatePEM
			out.ServingPrivateKey = cert.PrivateKeyPEM
			out.CACertificate = cert.caPEM
		}

		if rec.Bootstrap.Consumed {
			return out, nil
		}

		password := rec.Bootstrap.MasterUserPassword
		now := time.Now().UTC()
		rec.Bootstrap.MasterUserPassword = ""
		rec.Bootstrap.Consumed = true
		rec.Bootstrap.ConsumedAt = &now
		rec.UpdatedAt = now
		if err := updateJSON(ctx, kv, key, rev, &rec); err != nil {
			if !errors.Is(err, jetstream.ErrKeyExists) {
				return nil, fmt.Errorf("rds bootstrap: consume master password for %s: %w",
					input.DBInstanceIdentifier, err)
			}
			// A revision conflict may be an unrelated record update. Re-read and
			// return attach only when the stored consumed marker proves another
			// bootstrap fetch won the password.
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			continue
		}
		out.Mode = BootstrapModeInitialize
		out.MasterUserPassword = &password
		return out, nil
	}
}

// bootstrapCert bundles a minted serving cert with the CA the agent must trust.
type bootstrapCert struct {
	*ServingCert

	caPEM string
}

// mintServingCert issues a fresh serving cert for the instance. A deployment
// with no cluster CA may boot without TLS, but configured TLS fails closed when
// the instance record has no ENI address for the required IP SAN.
func (s *Service) mintServingCert(rec *DBInstanceRecord) (*bootstrapCert, error) {
	caCert, caKey, err := s.loadCA()
	if err != nil {
		return nil, fmt.Errorf("rds bootstrap: load cluster CA: %w", err)
	}
	if caCert == nil || caKey == nil {
		return nil, nil
	}
	if rec.ENIPrivateIP == "" {
		return nil, errors.New("rds bootstrap: configured TLS requires an ENI private IP")
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
// backs, so an agent's system-account credentials resolve with one Get instead
// of a scan across every per-account bucket.
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
