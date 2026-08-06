package handlers_rds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go/jetstream"
)

// The staged master password is encrypted at rest under bootstrap-payloads/{id}
// and replayed to the bound VM generation until the guest proves PostgreSQL
// applied it. The protocol is encrypt → replay → durably apply → acknowledge →
// delete, because no KV mutation can be made atomic with a network delivery.

const (
	bootstrapEnvelopeVersion = 1
	bootstrapPayloadIDPrefix = "bp"
)

// The cleartext wrapper, so a stale generation or a payload written under
// another master key is rejected with a legible reason rather than as a GCM
// authentication failure that reads as tampering.
type BootstrapPayloadEnvelope struct {
	PayloadID         string    `json:"payloadId"`
	EnvelopeVersion   int       `json:"envelopeVersion"`
	KeyID             string    `json:"keyId"`
	BoundVMGeneration int64     `json:"boundVmGeneration"`
	CreatedAt         time.Time `json:"createdAt"`
	EncryptedPayload  string    `json:"encryptedPayload"`
}

// The plaintext inside EncryptedPayload. handlers_iam.EncryptSecret takes no
// additional authenticated data, so every claim is carried here and validated
// after decrypting — ciphertext copied between accounts, instances or
// generations then fails on a claim rather than being accepted.
//
// DbiResourceID is the load-bearing one: deleting db1 and recreating it yields a
// record with the same identifier and the same first generation, and only the
// resource ID tells the two apart.
type bootstrapPayloadClaims struct {
	EnvelopeVersion      int    `json:"envelopeVersion"`
	KeyID                string `json:"keyId"`
	AccountID            string `json:"accountId"`
	DBInstanceIdentifier string `json:"dbInstanceIdentifier"`
	DbiResourceID        string `json:"dbiResourceId"`
	BoundVMGeneration    int64  `json:"boundVmGeneration"`
	PayloadID            string `json:"payloadId"`
	MasterUsername       string `json:"masterUsername"`
	MasterPassword       string `json:"masterPassword"`
}

// Required at service construction rather than per operation: the fetch is
// answered by whichever daemon the spinifex-workers queue group picks, so a
// per-operation dependency would make a pending fetch succeed or fail depending
// on which node answered.
func (s *Service) masterKey() ([]byte, error) {
	if len(s.deps.MasterKey) == 0 {
		return nil, errors.New("rds bootstrap: no master key is configured")
	}
	return s.deps.MasterKey, nil
}

// Diagnostic only. Key rotation is not supported by any service in the platform;
// this exists so a payload written under a different key reports that rather
// than failing as a corrupt ciphertext.
func bootstrapKeyID(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:8])
}

// Stages the master password for rec's current VM generation. The key's presence
// is what makes the bootstrap pending, so it is written once and only ever
// deleted by an acknowledgement, a delete, or a generation bump.
func (s *Service) writeBootstrapPayload(ctx context.Context, kv jetstream.KeyValue,
	accountID string, rec *DBInstanceRecord, masterPassword string) (string, error) {
	key, err := s.masterKey()
	if err != nil {
		return "", err
	}
	keyID := bootstrapKeyID(key)
	payloadID := utils.GenerateResourceID(bootstrapPayloadIDPrefix)

	plaintext, err := json.Marshal(bootstrapPayloadClaims{
		EnvelopeVersion:      bootstrapEnvelopeVersion,
		KeyID:                keyID,
		AccountID:            accountID,
		DBInstanceIdentifier: rec.DBInstanceIdentifier,
		DbiResourceID:        rec.DbiResourceID,
		BoundVMGeneration:    rec.VMGeneration,
		PayloadID:            payloadID,
		MasterUsername:       rec.MasterUsername,
		MasterPassword:       masterPassword,
	})
	if err != nil {
		return "", fmt.Errorf("rds bootstrap: marshal the payload for %s: %w", rec.DBInstanceIdentifier, err)
	}
	ciphertext, err := handlers_iam.EncryptSecret(string(plaintext), key)
	if err != nil {
		return "", fmt.Errorf("rds bootstrap: encrypt the payload for %s: %w", rec.DBInstanceIdentifier, err)
	}

	envelope := BootstrapPayloadEnvelope{
		PayloadID:         payloadID,
		EnvelopeVersion:   bootstrapEnvelopeVersion,
		KeyID:             keyID,
		BoundVMGeneration: rec.VMGeneration,
		CreatedAt:         time.Now().UTC(),
		EncryptedPayload:  ciphertext,
	}
	if err := createJSON(ctx, kv, BootstrapPayloadKey(rec.DBInstanceIdentifier), envelope); err != nil {
		return "", fmt.Errorf("rds bootstrap: stage the payload for %s: %w", rec.DBInstanceIdentifier, err)
	}
	return payloadID, nil
}

// Returns (nil, 0, nil) when nothing is staged, which is what an acknowledged,
// restored or legacy instance reads as.
func readBootstrapPayload(ctx context.Context, kv jetstream.KeyValue,
	dbInstanceIdentifier string) (*BootstrapPayloadEnvelope, uint64, error) {
	var envelope BootstrapPayloadEnvelope
	rev, found, err := getJSONRevision(ctx, kv, BootstrapPayloadKey(dbInstanceIdentifier), &envelope)
	if err != nil || !found {
		return nil, 0, err
	}
	return &envelope, rev, nil
}

// An already-absent key is the state this is trying to reach, so it is not an
// error.
func deleteBootstrapPayload(ctx context.Context, kv jetstream.KeyValue,
	dbInstanceIdentifier string, opts ...jetstream.KVDeleteOpt) error {
	err := kv.Delete(ctx, BootstrapPayloadKey(dbInstanceIdentifier), opts...)
	if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("rds bootstrap: delete the payload for %s: %w", dbInstanceIdentifier, err)
	}
	return nil
}

// Whether a staged payload is still waiting to be applied. Callers that only
// need the yes/no — the modify gate, the reconciler's stale-payload sweep — use
// this rather than decrypting.
func bootstrapPending(ctx context.Context, kv jetstream.KeyValue, dbInstanceIdentifier string) (bool, error) {
	envelope, _, err := readBootstrapPayload(ctx, kv, dbInstanceIdentifier)
	return envelope != nil, err
}

// The payload key first, the record second. A record whose payload is gone and
// which carries none of the markers below never had one staged.
func resolveBootstrapState(rec *DBInstanceRecord, payloadPresent bool) string {
	switch {
	case payloadPresent:
		return BootstrapStatePending
	case rec.Bootstrap.AcknowledgedAt != nil:
		return BootstrapStateAcknowledged
	case rec.Bootstrap.Consumed:
		return BootstrapStateLegacyConsumed
	case rec.Bootstrap.State == BootstrapStateUnrecoverable:
		return BootstrapStateUnrecoverable
	default:
		return BootstrapStateNone
	}
}

// Decrypts and validates every claim against the record it was staged for.
// Returns an AccessDenied-classed error for a claim mismatch and a plain error
// for a payload this daemon cannot read at all.
func (s *Service) openBootstrapPayload(envelope *BootstrapPayloadEnvelope, accountID string,
	rec *DBInstanceRecord) (*bootstrapPayloadClaims, error) {
	key, err := s.masterKey()
	if err != nil {
		return nil, err
	}
	if envelope.EnvelopeVersion != bootstrapEnvelopeVersion {
		return nil, fmt.Errorf("rds bootstrap: payload for %s is envelope version %d, this daemon serves version %d",
			rec.DBInstanceIdentifier, envelope.EnvelopeVersion, bootstrapEnvelopeVersion)
	}
	if keyID := bootstrapKeyID(key); envelope.KeyID != keyID {
		return nil, fmt.Errorf("rds bootstrap: payload for %s was encrypted under master key %s, this daemon holds %s; key rotation is not supported",
			rec.DBInstanceIdentifier, envelope.KeyID, keyID)
	}

	plaintext, err := handlers_iam.DecryptSecret(envelope.EncryptedPayload, key)
	if err != nil {
		return nil, fmt.Errorf("rds bootstrap: decrypt the payload for %s: %w", rec.DBInstanceIdentifier, err)
	}
	var claims bootstrapPayloadClaims
	if err := json.Unmarshal([]byte(plaintext), &claims); err != nil {
		return nil, fmt.Errorf("rds bootstrap: unmarshal the payload for %s: %w", rec.DBInstanceIdentifier, err)
	}

	// Every claim, not just the ones the outer envelope already showed: the
	// envelope is unauthenticated, so only these prove the ciphertext was
	// written for this account, this instance and this generation.
	switch {
	case claims.EnvelopeVersion != envelope.EnvelopeVersion,
		claims.KeyID != envelope.KeyID,
		claims.PayloadID != envelope.PayloadID,
		claims.BoundVMGeneration != envelope.BoundVMGeneration,
		claims.AccountID != accountID,
		claims.DBInstanceIdentifier != rec.DBInstanceIdentifier,
		claims.DbiResourceID != rec.DbiResourceID,
		claims.BoundVMGeneration != rec.VMGeneration:
		return nil, awserrors.Errorf(awserrors.ErrorAccessDenied,
			"the staged bootstrap payload for %s does not belong to this DB instance generation", rec.DBInstanceIdentifier)
	}
	if claims.MasterPassword == "" {
		return nil, fmt.Errorf("rds bootstrap: payload for %s carries no master password", rec.DBInstanceIdentifier)
	}
	return &claims, nil
}

// Records that the staged password can no longer reach this datadir. Written
// rather than inferred so the reason rides DescribeDBInstances, and idempotent
// so a repeated fetch does not churn the record.
func (s *Service) markBootstrapUnrecoverable(ctx context.Context, kv jetstream.KeyValue,
	accountID, dbInstanceIdentifier, reason string) error {
	changed := false
	if err := s.updateInstance(ctx, kv, dbInstanceIdentifier, func(stored *DBInstanceRecord) {
		if stored.Bootstrap.State == BootstrapStateUnrecoverable && stored.Bootstrap.MasterUserPassword == "" {
			return
		}
		changed = true
		stored.Bootstrap.MasterUserPassword = ""
		stored.Bootstrap.State = BootstrapStateUnrecoverable
		stored.FailureReason = reason
	}); err != nil {
		return fmt.Errorf("rds bootstrap: mark %s unrecoverable: %w", dbInstanceIdentifier, err)
	}
	if changed {
		s.RecordEvent(ctx, accountID, EventSourceTypeDBInstance, dbInstanceIdentifier,
			"The initial bootstrap of this DB instance cannot be completed: "+reason,
			EventCategoryFailure, EventCategoryNotification)
	}
	return nil
}

// The safety net for a generation bump on an instance that never finished its
// initial bootstrap. The payload is never rebound to the new generation: the
// password is not what stops the replacement working — replaceInstanceVM
// revokes the data-volume format grant, and only the initial create can hold
// one — so rebinding would move the failure rather than fix it.
func (s *Service) discardPendingBootstrap(ctx context.Context, kv jetstream.KeyValue,
	accountID string, rec *DBInstanceRecord) error {
	pending, err := bootstrapPending(ctx, kv, rec.DBInstanceIdentifier)
	if err != nil || !pending {
		return err
	}
	if err := deleteBootstrapPayload(ctx, kv, rec.DBInstanceIdentifier); err != nil {
		return err
	}
	rec.Bootstrap.State = BootstrapStateUnrecoverable
	return s.markBootstrapUnrecoverable(ctx, kv, accountID, rec.DBInstanceIdentifier,
		"the VM was replaced before the initial bootstrap completed; delete and recreate the DB instance")
}
