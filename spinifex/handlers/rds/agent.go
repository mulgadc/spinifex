package handlers_rds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// Bootstrap modes returned by GetDBBootstrapConfig. Initialize carries the
// master password and is replayed for as long as the payload stays staged;
// attach is the same payload minus the password, for a VM booting against an
// existing datadir.
const (
	BootstrapModeInitialize = "initialize"
	BootstrapModeAttach     = "attach"
)

// The agent protocol types below carry both json and locationName tags because
// the same value crosses NATS as JSON and leaves the gateway as Query-protocol
// XML. Optional fields are pointers so a nil renders as an absent element.

// InstanceID and DBInstanceIdentifier are set by the gateway from the caller's
// resolved identity, never from the agent's request body.
type RegisterDBInstanceInput struct {
	DBInstanceIdentifier string `json:"dbInstanceIdentifier"`
	InstanceID           string `json:"instanceId"`
	AgentVersion         string `json:"agentVersion,omitempty"`
	EngineVersion        string `json:"engineVersion,omitempty"`
}

// The heartbeat cadence is handed back on registration so the interval stays
// control-plane-owned.
type RegisterDBInstanceOutput struct {
	DBInstanceIdentifier     string `json:"dbInstanceIdentifier" locationName:"DBInstanceIdentifier"`
	HeartbeatIntervalSeconds int64  `json:"heartbeatIntervalSeconds" locationName:"HeartbeatIntervalSeconds"`
}

// The periodic beat folds liveness into the state report rather than using a
// separate heartbeat call, so a healthy instance costs one round trip per tick.
type SubmitDBStateChangeInput struct {
	DBInstanceIdentifier string       `json:"dbInstanceIdentifier"`
	InstanceID           string       `json:"instanceId"`
	EngineHealth         EngineHealth `json:"engineHealth"`
	EngineVersion        string       `json:"engineVersion,omitempty"`
	Message              string       `json:"message,omitempty"`
}

// Persisted reports whether the beat reached KV; diagnostic only, the agent
// behaves the same either way.
type SubmitDBStateChangeOutput struct {
	Acknowledged             bool  `json:"acknowledged" locationName:"Acknowledged"`
	Persisted                bool  `json:"persisted" locationName:"Persisted"`
	HeartbeatIntervalSeconds int64 `json:"heartbeatIntervalSeconds" locationName:"HeartbeatIntervalSeconds"`
}

type GetDBBootstrapConfigInput struct {
	DBInstanceIdentifier string `json:"dbInstanceIdentifier"`
	InstanceID           string `json:"instanceId"`
	VMGeneration         int64  `json:"vmGeneration"`
}

// Everything rds-init needs to bootstrap or attach. The serving cert and key
// are minted per call and never persisted.
type GetDBBootstrapConfigOutput struct {
	Mode                 string  `json:"mode" locationName:"Mode"`
	DBInstanceIdentifier string  `json:"dbInstanceIdentifier" locationName:"DBInstanceIdentifier"`
	Engine               string  `json:"engine" locationName:"Engine"`
	EngineVersion        string  `json:"engineVersion,omitempty" locationName:"EngineVersion"`
	DBName               string  `json:"dbName,omitempty" locationName:"DBName"`
	MasterUsername       string  `json:"masterUsername" locationName:"MasterUsername"`
	MasterUserPassword   *string `json:"masterUserPassword,omitempty" locationName:"MasterUserPassword"`
	Port                 int64   `json:"port" locationName:"Port"`

	DataVolumeID     string `json:"dataVolumeId,omitempty" locationName:"DataVolumeId" xml:"DataVolumeId"`
	DataVolumeSerial string `json:"dataVolumeSerial,omitempty" locationName:"DataVolumeSerial" xml:"DataVolumeSerial"`
	VMGeneration     int64  `json:"vmGeneration,omitempty" locationName:"VMGeneration" xml:"VMGeneration"`
	FormatAuthorized bool   `json:"formatAuthorized" locationName:"FormatAuthorized" xml:"FormatAuthorized"`

	Parameters []Parameter `json:"parameters,omitempty" locationName:"Parameters" locationNameList:"member" xml:"Parameters>member"`

	// Names the staged payload this response replayed and asserts that the
	// control plane is still waiting for it to be applied. The guest carries both
	// into the handoff, where rds-init uses them to refuse a datadir whose master
	// role it cannot prove.
	PayloadID        string `json:"payloadId,omitempty" locationName:"PayloadId" xml:"PayloadId"`
	BootstrapPending bool   `json:"bootstrapPending" locationName:"BootstrapPending" xml:"BootstrapPending"`

	// Empty when no cluster CA is configured; the agent then starts the engine
	// without TLS rather than failing to boot, since TLS is offered not enforced.
	ServingCertificate string `json:"servingCertificate,omitempty" locationName:"ServingCertificate"`
	ServingPrivateKey  string `json:"servingPrivateKey,omitempty" locationName:"ServingPrivateKey"`
	CACertificate      string `json:"caCertificate,omitempty" locationName:"CACertificate"`
}

// Reports that PostgreSQL durably applied the staged master password, which is
// the only thing that destroys the ciphertext. Distinct from SubmitDBStateChange
// because a heartbeat is best-effort while this must be retried until confirmed
// and denied on an identity mismatch.
//
// DBInstanceIdentifier and InstanceID are set by the gateway from the caller's
// resolved identity; the rest is the guest's own assertion, checked here.
type AcknowledgeDBBootstrapInput struct {
	DBInstanceIdentifier string `json:"dbInstanceIdentifier"`
	InstanceID           string `json:"instanceId"`
	PayloadID            string `json:"payloadId"`
	VMGeneration         int64  `json:"vmGeneration"`
	DataVolumeID         string `json:"dataVolumeId,omitempty"`
}

// Minimal by design: the agent needs only success to decide it may remove its
// completion receipt.
type AcknowledgeDBBootstrapOutput struct {
	Acknowledged   bool       `json:"acknowledged" locationName:"Acknowledged"`
	AcknowledgedAt *time.Time `json:"acknowledgedAt,omitempty" locationName:"AcknowledgedAt" type:"timestamp"`
}

// One directive delivered to an agent on its long poll. Concrete types land
// with their owning phases — password apply, parameter reload, grow, quiesce.
type Command struct {
	CommandID  string      `json:"commandId" locationName:"CommandId" xml:"CommandId"`
	Type       string      `json:"type" locationName:"Type"`
	Parameters []Parameter `json:"parameters,omitempty" locationName:"Parameters" locationNameList:"member" xml:"Parameters>member"`
	IssuedAt   *time.Time  `json:"issuedAt,omitempty" locationName:"IssuedAt" type:"timestamp"`
}

// The agent's result for a command from an earlier poll, carried on the next
// poll request and republished to the issuer.
type CommandReply struct {
	CommandID string `json:"commandId" locationName:"CommandId" xml:"CommandId"`
	Status    string `json:"status" locationName:"Status"`
	Message   string `json:"message,omitempty" locationName:"Message"`
}

const (
	CommandStatusSucceeded = "succeeded"
	CommandStatusFailed    = "failed"

	// ParameterRollbackMessage is sent before the guest restarts on the last
	// serving parameter set. The control plane keeps the group out of in-sync
	// until a later apply succeeds.
	ParameterRollbackMessage = "engine did not start after a parameter change; rolled back to the last accepted set"
)

// Idempotent: re-registering is the normal case after an agent restart and
// simply refreshes the record.
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
	// RegisteredAt marks the first registration of this VM, so a replace with a
	// new instance ID resets it rather than carrying it forward.
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

// Persists on a change of health or message and on the slower floor, holding
// intermediate beats in memory so a steady fleet stays off the KV hot path.
func (s *Service) SubmitDBStateChange(ctx context.Context, input *SubmitDBStateChangeInput, accountID string) (*SubmitDBStateChangeOutput, error) {
	if input.DBInstanceIdentifier == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if !ValidEngineHealth(input.EngineHealth) {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"unknown engine health %q", input.EngineHealth)
	}

	parameterRollback := input.Message == ParameterRollbackMessage
	beat, persist := s.noteBeat(accountID, input.DBInstanceIdentifier, input.EngineHealth, input.Message, parameterRollback)
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
	newParameterRollback := parameterRollback && !rec.ParametersRolledBack
	if parameterRollback {
		rec.ParametersRolledBack = true
	}
	if input.EngineVersion != "" {
		rec.Agent.EngineVersion = input.EngineVersion
	}
	// Stamped from the beat itself rather than from the write it triggered, so
	// the persisted clock never reads newer than the in-memory one recording the
	// same beat — which would cost the reconciler the tighter staleness bound.
	rec.Agent.LastSeen = &beat
	rec.UpdatedAt = now

	if err := updateJSON(ctx, kv, key, rev, &rec); err != nil {
		return nil, err
	}
	out.Persisted = true
	if newParameterRollback {
		s.RecordEvent(ctx, accountID, EventSourceTypeDBInstance, rec.DBInstanceIdentifier,
			"The database engine rejected its pending parameters and restarted on the last accepted set.",
			EventCategoryConfigurationChange, EventCategoryFailure)
	}
	return out, nil
}

// Records the beat and reports when it must reach KV: a changed health or
// message persists immediately, an unchanged one only every
// HeartbeatPersistEvery beats. The beat's own time is returned so a persisting
// caller stamps the record with it rather than with a later clock reading.
func (s *Service) noteBeat(accountID, dbID string, health EngineHealth, message string, force bool) (time.Time, bool) {
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
		return live.lastSeen, true
	}
	return live.lastSeen, false
}

// The in-memory beat time, fresher than the record's persisted LastSeen. False
// means this node saw no beat — normal after a leader change; fall back to KV.
func (s *Service) LastSeen(accountID, dbID string) (time.Time, bool) {
	s.livenessMu.Lock()
	defer s.livenessMu.Unlock()
	live, ok := s.liveness[accountID+"/"+dbID]
	if !ok || live.lastSeen.IsZero() {
		return time.Time{}, false
	}
	return live.lastSeen, true
}

// Serves boot material and, while a payload is staged for the caller's
// generation, replays the master password. Nothing is mutated on the way, so a
// dropped reply, a gateway restart or a reboot before the guest applied the
// password all recover by simply asking again.
func (s *Service) GetDBBootstrapConfig(ctx context.Context, input *GetDBBootstrapConfigInput, accountID string) (*GetDBBootstrapConfigOutput, error) {
	if input.DBInstanceIdentifier == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}

	var rec DBInstanceRecord
	found, err := getJSON(ctx, kv, DBInstanceKey(input.DBInstanceIdentifier), &rec)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New(awserrors.ErrorDBInstanceNotFound)
	}
	if rec.InstanceID == "" || input.InstanceID != rec.InstanceID {
		return nil, errors.New(awserrors.ErrorAccessDenied)
	}

	generationMatches := input.VMGeneration > 0 && input.VMGeneration == rec.VMGeneration
	// An old gateway may omit the generation for an existing record. It may
	// still serve attach material, but it must not expose a live create grant or
	// a staged password until the caller is bound to the current generation.
	if rec.FormatAuthorized && !generationMatches {
		return nil, errors.New(awserrors.ErrorAccessDenied)
	}
	serial := rec.DataVolumeSerial
	if serial == "" && rec.DataVolumeID != "" {
		// Backward-compatible identity for records written before the serial
		// field existed. Missing authorization remains false.
		serial = vm.VolumeSerial(rec.DataVolumeID)
	}
	authoritativeSerial := vm.VolumeSerial(rec.DataVolumeID)
	grantIdentityValid := generationMatches && rec.DataVolumeID != "" &&
		rec.DataVolumeSerial != "" && rec.DataVolumeSerial == authoritativeSerial
	if rec.FormatAuthorized && !grantIdentityValid {
		return nil, errors.New(awserrors.ErrorAccessDenied)
	}

	out := &GetDBBootstrapConfigOutput{
		Mode:                 BootstrapModeAttach,
		DBInstanceIdentifier: rec.DBInstanceIdentifier,
		Engine:               rec.Engine,
		EngineVersion:        rec.EngineVersion,
		DBName:               rec.DBName,
		MasterUsername:       rec.MasterUsername,
		Port:                 rec.Port,
		DataVolumeID:         rec.DataVolumeID,
		DataVolumeSerial:     serial,
		VMGeneration:         rec.VMGeneration,
		FormatAuthorized:     rec.FormatAuthorized && grantIdentityValid,
		Parameters:           rec.Bootstrap.ResolvedParameters,
	}
	// Minted per call and never persisted, so every boot — attach included —
	// gets fresh serving material.
	cert, err := s.mintServingCert(&rec)
	if err != nil {
		return nil, err
	}
	if cert != nil {
		out.ServingCertificate = cert.CertificatePEM
		out.ServingPrivateKey = cert.PrivateKeyPEM
		out.CACertificate = cert.caPEM
	}

	if err := s.applyStagedBootstrap(ctx, kv, accountID, &rec, generationMatches, out); err != nil {
		return nil, err
	}
	return out, nil
}

// Fills in the initialize half of the response when a payload is staged for this
// generation, and leaves the attach response untouched otherwise.
//
// A payload this daemon cannot open is reported as unrecoverable and still
// answered with attach: failing the fetch outright would leave a VM retrying
// with no diagnostic channel, whereas an attach lets it boot, register and beat
// so the operator sees a running-but-broken instance. The guest guard fails
// closed on the datadir either way.
//
// Recording that verdict is bookkeeping, so a failure to write it is logged
// rather than returned. Propagating it would discard an attach response this
// call has already built and reinstate the retry loop it exists to avoid; the
// verdict is idempotent, so the next fetch records it.
func (s *Service) applyStagedBootstrap(ctx context.Context, kv jetstream.KeyValue, accountID string,
	rec *DBInstanceRecord, generationMatches bool, out *GetDBBootstrapConfigOutput) error {
	envelope, _, err := readBootstrapPayload(ctx, kv, rec.DBInstanceIdentifier)
	if err != nil {
		return err
	}
	if envelope == nil {
		if err := s.scrubLegacyBootstrap(ctx, kv, accountID, rec); err != nil {
			slog.ErrorContext(ctx, "rds bootstrap: a legacy plaintext password could not be scrubbed; serving attach regardless",
				"dbInstance", rec.DBInstanceIdentifier, "err", err)
		}
		return nil
	}
	// A superseded VM gets the same attach material as any other boot but never
	// the staged password.
	if !generationMatches || envelope.BoundVMGeneration != rec.VMGeneration {
		slog.WarnContext(ctx, "rds bootstrap: a staged payload was not served to a caller outside its bound generation",
			"dbInstance", rec.DBInstanceIdentifier, "recordGeneration", rec.VMGeneration,
			"boundGeneration", envelope.BoundVMGeneration)
		return nil
	}

	claims, err := s.openBootstrapPayload(envelope, accountID, rec)
	if err != nil {
		slog.ErrorContext(ctx, "rds bootstrap: the staged payload could not be opened",
			"dbInstance", rec.DBInstanceIdentifier, "err", err)
		if err := s.markBootstrapUnrecoverable(ctx, kv, accountID, rec.DBInstanceIdentifier,
			"the staged bootstrap payload cannot be read by the control plane; delete and recreate the DB instance"); err != nil {
			slog.ErrorContext(ctx, "rds bootstrap: the unrecoverable verdict could not be recorded; serving attach regardless",
				"dbInstance", rec.DBInstanceIdentifier, "err", err)
		}
		return nil
	}

	password := claims.MasterPassword
	out.Mode = BootstrapModeInitialize
	out.MasterUsername = claims.MasterUsername
	out.MasterUserPassword = &password
	out.PayloadID = claims.PayloadID
	out.BootstrapPending = true
	return nil
}

// Beta records staged the password as cleartext on the record itself. One that
// was never consumed also never bootstrapped, so there is no datadir to save:
// the password is removed on sight and the instance is marked for recreation
// rather than left holding a secret at rest for an operator who may never act.
func (s *Service) scrubLegacyBootstrap(ctx context.Context, kv jetstream.KeyValue,
	accountID string, rec *DBInstanceRecord) error {
	if rec.Bootstrap.MasterUserPassword == "" {
		return nil
	}
	rec.Bootstrap.MasterUserPassword = ""
	return s.markBootstrapUnrecoverable(ctx, kv, accountID, rec.DBInstanceIdentifier,
		"this DB instance staged its master password before the password was encrypted at rest and never completed its initial bootstrap; delete and recreate it")
}

// Destroys the staged ciphertext once the guest has proven PostgreSQL applied
// it. Deliberately never decrypts: a lost acknowledgement has to remain
// deliverable even after a master.key change made the payload unreadable, so the
// one operation that destroys key material must not depend on reading it.
func (s *Service) AcknowledgeDBBootstrap(ctx context.Context, input *AcknowledgeDBBootstrapInput,
	accountID string) (*AcknowledgeDBBootstrapOutput, error) {
	if input.DBInstanceIdentifier == "" || input.PayloadID == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	rec, _, err := s.getDBInstance(ctx, kv, input.DBInstanceIdentifier)
	if err != nil {
		return nil, err
	}
	if rec.InstanceID == "" || input.InstanceID != rec.InstanceID {
		return nil, errors.New(awserrors.ErrorAccessDenied)
	}

	envelope, rev, err := readBootstrapPayload(ctx, kv, input.DBInstanceIdentifier)
	if err != nil {
		return nil, err
	}
	if envelope == nil {
		// The true duplicate: the first acknowledgement landed and its response
		// was lost. The retained payload ID is the only thing left to answer it
		// with, which is why it outlives the key it names.
		if rec.Bootstrap.PayloadID != "" && rec.Bootstrap.PayloadID == input.PayloadID {
			return &AcknowledgeDBBootstrapOutput{
				Acknowledged: true, AcknowledgedAt: rec.Bootstrap.AcknowledgedAt,
			}, nil
		}
		return nil, awserrors.Errorf(awserrors.ErrorAccessDenied,
			"no bootstrap payload %s is or was staged for DB instance %s", input.PayloadID, input.DBInstanceIdentifier)
	}

	// A superseded VM must never acknowledge: it would destroy the payload the
	// current generation is still waiting to replay.
	if envelope.PayloadID != input.PayloadID || envelope.BoundVMGeneration != rec.VMGeneration ||
		input.VMGeneration != rec.VMGeneration {
		return nil, awserrors.Errorf(awserrors.ErrorAccessDenied,
			"bootstrap payload %s at generation %d does not match the current state of DB instance %s",
			input.PayloadID, input.VMGeneration, input.DBInstanceIdentifier)
	}
	// An echo of what the fetch handed the guest rather than independent proof:
	// the agent does not observe the serial of the device rds-datadir mounted.
	// A mismatch is a platform bug worth denying on all the same.
	if input.DataVolumeID != "" && input.DataVolumeID != rec.DataVolumeID {
		return nil, awserrors.Errorf(awserrors.ErrorAccessDenied,
			"DB instance %s is not backed by data volume %s", input.DBInstanceIdentifier, input.DataVolumeID)
	}

	// The record first: a failure between the two leaves the payload staged and
	// the retry converges. Deleting first would leave a state where neither the
	// key nor the retained payload ID can answer the retry.
	now := time.Now().UTC()
	if err := s.updateInstance(ctx, kv, input.DBInstanceIdentifier, func(stored *DBInstanceRecord) {
		stored.Bootstrap.PayloadID = input.PayloadID
		stored.Bootstrap.State = BootstrapStateAcknowledged
		stored.Bootstrap.AcknowledgedAt = &now
		stored.Bootstrap.MasterUserPassword = ""
		// A daemon that could not read the payload marked this instance before
		// another one served it, so the stale verdict goes with the ciphertext.
		stored.Bootstrap.FailureReason = ""
	}); err != nil {
		return nil, err
	}
	if err := deleteBootstrapPayload(ctx, kv, input.DBInstanceIdentifier, jetstream.LastRevision(rev)); err != nil {
		return nil, err
	}

	slog.InfoContext(ctx, "rds: initial bootstrap acknowledged",
		"dbInstance", input.DBInstanceIdentifier, "accountId", accountID,
		"instanceId", input.InstanceID, "payloadId", input.PayloadID)
	s.RecordEvent(ctx, accountID, EventSourceTypeDBInstance, input.DBInstanceIdentifier,
		"The database engine confirmed the initial master credentials were applied.",
		EventCategoryCreation, EventCategoryNotification)

	return &AcknowledgeDBBootstrapOutput{Acknowledged: true, AcknowledgedAt: &now}, nil
}

type bootstrapCert struct {
	*ServingCert

	caPEM string
}

// A deployment with no cluster CA may boot without TLS, but configured TLS
// fails closed when the record has no ENI address for the required IP SAN.
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

// Maps an internal EC2 instance ID to the DB instance it backs, so an agent's
// credentials resolve with one Get instead of a scan across every bucket.
type InstanceIndexEntry struct {
	AccountID            string `json:"accountId"`
	DBInstanceIdentifier string `json:"dbInstanceIdentifier"`
	// Increments on every replace, so a superseded VM's agent is
	// distinguishable from the current one.
	VMGeneration int64 `json:"vmGeneration"`
}

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

func (s *Service) PutInstanceIndex(ctx context.Context, instanceID string, entry InstanceIndexEntry) error {
	kv, err := s.systemBucket(ctx)
	if err != nil {
		return err
	}
	return putJSON(ctx, kv, InstanceIndexKey(instanceID), entry)
}

func (s *Service) DeleteInstanceIndex(ctx context.Context, instanceID string) error {
	kv, err := s.systemBucket(ctx)
	if err != nil {
		return err
	}
	return kv.Delete(ctx, InstanceIndexKey(instanceID))
}
