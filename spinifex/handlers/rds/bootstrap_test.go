package handlers_rds

import (
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A fixed 32-byte AES-256 key, so a ciphertext written by one harness is
// readable by another and a wrong-key case is expressible.
var testMasterKey = []byte("0123456789abcdef0123456789abcdef")

const (
	testDbiResourceID   = "db-AAAAAAAAAAAAAAAAA"
	testMasterPassword  = "s3cr3t-master-pw"
	testOtherMasterKeyS = "fedcba9876543210fedcba9876543210"
)

// stagePayload writes the encrypted payload a create would have staged for rec,
// and returns its ID.
func stagePayload(t *testing.T, svc *Service, rec *DBInstanceRecord) string {
	t.Helper()
	kv, err := svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	payloadID, err := svc.writeBootstrapPayload(t.Context(), kv, testAccountID, rec, testMasterPassword)
	require.NoError(t, err)
	return payloadID
}

// seedPending seeds the record and stages its payload, which together are what a
// finished CreateDBInstance leaves behind.
func seedPending(t *testing.T, svc *Service, rec DBInstanceRecord) string {
	t.Helper()
	seedInstance(t, svc, rec)
	return stagePayload(t, svc, &rec)
}

func storedPayload(t *testing.T, svc *Service, id string) (*BootstrapPayloadEnvelope, string) {
	t.Helper()
	kv, err := svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	entry, err := kv.Get(t.Context(), BootstrapPayloadKey(id))
	if err != nil {
		return nil, ""
	}
	var envelope BootstrapPayloadEnvelope
	require.NoError(t, json.Unmarshal(entry.Value(), &envelope))
	return &envelope, string(entry.Value())
}

func TestBootstrapPayload_RoundTripsAndCarriesNoCleartext(t *testing.T) {
	svc := newTestService(t)
	rec := defaultRecord()
	seedPending(t, svc, rec)

	envelope, raw := storedPayload(t, svc, testDBID)
	require.NotNil(t, envelope)
	assert.NotContains(t, raw, testMasterPassword, "the staged password must never be readable at rest")
	assert.Equal(t, bootstrapEnvelopeVersion, envelope.EnvelopeVersion)
	assert.Equal(t, bootstrapKeyID(testMasterKey), envelope.KeyID)
	assert.Equal(t, rec.VMGeneration, envelope.BoundVMGeneration)

	claims, err := svc.openBootstrapPayload(envelope, testAccountID, &rec)
	require.NoError(t, err)
	assert.Equal(t, testMasterPassword, claims.MasterPassword)
	assert.Equal(t, rec.MasterUsername, claims.MasterUsername)
	assert.Equal(t, rec.DbiResourceID, claims.DbiResourceID)
}

// The same password staged twice must not produce the same bytes, or a KV reader
// could tell two instances share a password without decrypting either.
func TestBootstrapPayload_CiphertextIsNotDeterministic(t *testing.T) {
	svc := newTestService(t)
	rec := defaultRecord()
	seedInstance(t, svc, rec)

	kv, err := svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	first, err := svc.writeBootstrapPayload(t.Context(), kv, testAccountID, &rec, testMasterPassword)
	require.NoError(t, err)
	one, _ := storedPayload(t, svc, testDBID)
	require.NoError(t, deleteBootstrapPayload(t.Context(), kv, testDBID))
	second, err := svc.writeBootstrapPayload(t.Context(), kv, testAccountID, &rec, testMasterPassword)
	require.NoError(t, err)
	two, _ := storedPayload(t, svc, testDBID)

	assert.NotEqual(t, first, second, "each staging mints its own payload ID")
	assert.NotEqual(t, one.EncryptedPayload, two.EncryptedPayload)
}

func TestBootstrapPayload_TamperedCiphertextIsRejected(t *testing.T) {
	svc := newTestService(t)
	rec := defaultRecord()
	seedPending(t, svc, rec)

	envelope, _ := storedPayload(t, svc, testDBID)
	// Flip one base64 character; GCM authenticates the whole thing, so any edit
	// fails rather than yielding a plausible plaintext.
	body := []byte(envelope.EncryptedPayload)
	if body[10] == 'A' {
		body[10] = 'B'
	} else {
		body[10] = 'A'
	}
	envelope.EncryptedPayload = string(body)

	_, err := svc.openBootstrapPayload(envelope, testAccountID, &rec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decrypt the payload")
}

// A payload written under another master key must say so rather than surfacing a
// raw GCM authentication failure, which reads as tampering.
func TestBootstrapPayload_WrongMasterKeyIsReportedLegibly(t *testing.T) {
	svc := newTestService(t)
	rec := defaultRecord()
	seedPending(t, svc, rec)
	envelope, _ := storedPayload(t, svc, testDBID)

	rotated := svc.WithDeps(Deps{MasterKey: []byte(testOtherMasterKeyS)})
	_, err := rotated.openBootstrapPayload(envelope, testAccountID, &rec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "key rotation is not supported")
	assert.NotContains(t, err.Error(), testMasterPassword)
}

// Every claim is validated after decrypting, so ciphertext lifted from another
// account, instance, resource or generation is refused rather than replayed.
func TestBootstrapPayload_ClaimsBindItToOneInstanceGeneration(t *testing.T) {
	tests := []struct {
		name      string
		accountID string
		mutate    func(*DBInstanceRecord)
	}{
		{name: "another account", accountID: "999999999999"},
		{name: "another DB identifier", mutate: func(rec *DBInstanceRecord) {
			rec.DBInstanceIdentifier = "someone-elses-db"
		}},
		{name: "a recreate of the same identifier", mutate: func(rec *DBInstanceRecord) {
			rec.DbiResourceID = "db-BBBBBBBBBBBBBBBBB"
		}},
		{name: "a later VM generation", mutate: func(rec *DBInstanceRecord) {
			rec.VMGeneration = 2
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t)
			rec := defaultRecord()
			seedPending(t, svc, rec)
			envelope, _ := storedPayload(t, svc, testDBID)

			accountID := testAccountID
			if tc.accountID != "" {
				accountID = tc.accountID
			}
			target := rec
			if tc.mutate != nil {
				tc.mutate(&target)
			}

			_, err := svc.openBootstrapPayload(envelope, accountID, &target)
			require.Error(t, err)
			assert.Contains(t, err.Error(), awserrors.ErrorAccessDenied)
		})
	}
}

// The whole point of the split: a fetch serves the password without writing
// anything, so a lost reply is recovered by asking again.
func TestGetDBBootstrapConfig_ReplaysUntilAcknowledged(t *testing.T) {
	svc := newTestService(t)
	rec := defaultRecord()
	payloadID := seedPending(t, svc, rec)
	in := bootstrapInput()

	_, beforeRaw := readRecord(t, svc)
	for i := range 3 {
		out, err := svc.GetDBBootstrapConfig(t.Context(), in, testAccountID)
		require.NoError(t, err, "fetch %d", i)
		assert.Equal(t, BootstrapModeInitialize, out.Mode)
		require.NotNil(t, out.MasterUserPassword)
		assert.Equal(t, testMasterPassword, *out.MasterUserPassword)
		assert.Equal(t, payloadID, out.PayloadID)
		assert.True(t, out.BootstrapPending)
	}
	_, afterRaw := readRecord(t, svc)
	assert.Equal(t, beforeRaw, afterRaw, "a replayed fetch must mutate nothing")

	require.NoError(t, acknowledge(t, svc, payloadID))
	out, err := svc.GetDBBootstrapConfig(t.Context(), in, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, BootstrapModeAttach, out.Mode)
	assert.Nil(t, out.MasterUserPassword)
	assert.False(t, out.BootstrapPending)
}

func acknowledge(t *testing.T, svc *Service, payloadID string) error {
	t.Helper()
	_, err := svc.AcknowledgeDBBootstrap(t.Context(), &AcknowledgeDBBootstrapInput{
		DBInstanceIdentifier: testDBID,
		InstanceID:           testInstance,
		PayloadID:            payloadID,
		VMGeneration:         firstVMGeneration,
	}, testAccountID)
	return err
}

func TestAcknowledgeDBBootstrap_Resolution(t *testing.T) {
	tests := []struct {
		name      string
		prepare   func(t *testing.T, svc *Service, payloadID string) string
		wantErr   bool
		wantWiped bool
	}{
		{
			name:      "the payload is staged and every claim matches",
			prepare:   func(_ *testing.T, _ *Service, payloadID string) string { return payloadID },
			wantWiped: true,
		},
		{
			name: "a true duplicate after the key is already gone",
			prepare: func(t *testing.T, svc *Service, payloadID string) string {
				require.NoError(t, acknowledge(t, svc, payloadID))
				return payloadID
			},
			wantWiped: true,
		},
		{
			name: "no payload is or ever was staged for this ID",
			prepare: func(t *testing.T, svc *Service, payloadID string) string {
				require.NoError(t, acknowledge(t, svc, payloadID))
				return "bp-somebody-elses"
			},
			wantErr: true,
		},
		{
			name:    "the payload ID does not match the staged one",
			prepare: func(_ *testing.T, _ *Service, _ string) string { return "bp-stale" },
			wantErr: true,
		},
		{
			name: "a superseded VM acknowledging at an older generation",
			prepare: func(t *testing.T, svc *Service, payloadID string) string {
				kv, err := svc.bucket(t.Context(), testAccountID)
				require.NoError(t, err)
				require.NoError(t, svc.updateInstance(t.Context(), kv, testDBID, func(stored *DBInstanceRecord) {
					stored.VMGeneration = 2
				}))
				return payloadID
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t)
			payloadID := seedPending(t, svc, defaultRecord())
			requested := tc.prepare(t, svc, payloadID)

			out, err := svc.AcknowledgeDBBootstrap(t.Context(), &AcknowledgeDBBootstrapInput{
				DBInstanceIdentifier: testDBID,
				InstanceID:           testInstance,
				PayloadID:            requested,
				VMGeneration:         firstVMGeneration,
			}, testAccountID)
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), awserrors.ErrorAccessDenied)
				return
			}
			require.NoError(t, err)
			assert.True(t, out.Acknowledged)

			if tc.wantWiped {
				envelope, _ := storedPayload(t, svc, testDBID)
				assert.Nil(t, envelope, "a successful acknowledgement destroys the ciphertext")
				stored, _ := readRecord(t, svc)
				assert.Equal(t, BootstrapStateAcknowledged, stored.Bootstrap.State)
				assert.Equal(t, payloadID, stored.Bootstrap.PayloadID)
				require.NotNil(t, stored.Bootstrap.AcknowledgedAt)
			}
		})
	}
}

// The operation that destroys key material must not depend on being able to read
// it, or a master.key change would strand a bootstrapped instance forever.
func TestAcknowledgeDBBootstrap_NeverDecrypts(t *testing.T) {
	svc := newTestService(t)
	payloadID := seedPending(t, svc, defaultRecord())

	rotated := svc.WithDeps(Deps{LoadCA: newTestCA(t), MasterKey: []byte(testOtherMasterKeyS)})
	_, err := rotated.AcknowledgeDBBootstrap(t.Context(), &AcknowledgeDBBootstrapInput{
		DBInstanceIdentifier: testDBID,
		InstanceID:           testInstance,
		PayloadID:            payloadID,
		VMGeneration:         firstVMGeneration,
	}, testAccountID)
	require.NoError(t, err)

	envelope, _ := storedPayload(t, svc, testDBID)
	assert.Nil(t, envelope)
}

// The volume ID is an echo of what the fetch handed the guest rather than
// independent proof, but a mismatch is a platform bug worth denying on.
func TestAcknowledgeDBBootstrap_RejectsAForeignDataVolume(t *testing.T) {
	svc := newTestService(t)
	payloadID := seedPending(t, svc, defaultRecord())

	_, err := svc.AcknowledgeDBBootstrap(t.Context(), &AcknowledgeDBBootstrapInput{
		DBInstanceIdentifier: testDBID,
		InstanceID:           testInstance,
		PayloadID:            payloadID,
		VMGeneration:         firstVMGeneration,
		DataVolumeID:         "vol-somebody-else",
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorAccessDenied)

	envelope, _ := storedPayload(t, svc, testDBID)
	assert.NotNil(t, envelope, "a denied acknowledgement must leave the payload replayable")
}

// A beta record whose password was already spent keeps attaching: it is serving
// real data and holds no secret at rest to fix.
func TestGetDBBootstrapConfig_LegacyConsumedRecordAttaches(t *testing.T) {
	svc := newTestService(t)
	rec := defaultRecord()
	rec.Bootstrap = BootstrapState{Consumed: true, ResolvedParameters: rec.Bootstrap.ResolvedParameters}
	seedInstance(t, svc, rec)

	out, err := svc.GetDBBootstrapConfig(t.Context(), bootstrapInput(), testAccountID)
	require.NoError(t, err)
	assert.Equal(t, BootstrapModeAttach, out.Mode)
	assert.Nil(t, out.MasterUserPassword)

	stored, _ := readRecord(t, svc)
	assert.Equal(t, BootstrapStateLegacyConsumed, resolveBootstrapState(&stored))
	assert.Empty(t, stored.FailureReason, "an instance that is serving is not a failure")
}

// A beta record still holding cleartext never bootstrapped, so there is no
// datadir to save. The password is removed on sight rather than left waiting for
// an operator who may never act.
func TestGetDBBootstrapConfig_LegacyPlaintextIsScrubbedIdempotently(t *testing.T) {
	svc := newTestService(t)
	rec := defaultRecord()
	rec.Bootstrap.MasterUserPassword = "beta-plaintext-pw"
	seedInstance(t, svc, rec)

	for i := range 2 {
		out, err := svc.GetDBBootstrapConfig(t.Context(), bootstrapInput(), testAccountID)
		require.NoError(t, err, "fetch %d", i)
		assert.Equal(t, BootstrapModeAttach, out.Mode, "the initialize fetch is denied")
		assert.Nil(t, out.MasterUserPassword)

		stored, raw := readRecord(t, svc)
		assert.NotContains(t, raw, "beta-plaintext-pw")
		assert.Equal(t, BootstrapStateUnrecoverable, resolveBootstrapState(&stored))
		assert.Contains(t, stored.Bootstrap.FailureReason, "delete and recreate")
	}
}

// A payload this daemon cannot open still answers attach, so the VM boots,
// registers and beats and the operator sees a running-but-broken instance
// instead of a VM retrying into silence.
func TestGetDBBootstrapConfig_UnreadablePayloadAttachesAndReportsWhy(t *testing.T) {
	svc := newTestService(t)
	seedPending(t, svc, defaultRecord())

	rotated := svc.WithDeps(Deps{LoadCA: newTestCA(t), MasterKey: []byte(testOtherMasterKeyS)})
	out, err := rotated.GetDBBootstrapConfig(t.Context(), bootstrapInput(), testAccountID)
	require.NoError(t, err)
	assert.Equal(t, BootstrapModeAttach, out.Mode)
	assert.Nil(t, out.MasterUserPassword)
	assert.False(t, out.BootstrapPending)

	stored, _ := readRecord(t, svc)
	assert.Equal(t, BootstrapStateUnrecoverable, stored.Bootstrap.State)
	assert.Contains(t, stored.Bootstrap.FailureReason, "cannot be read by the control plane")
}

func TestCreateDBInstance_StagesThePasswordEncrypted(t *testing.T) {
	h := newCreateHarness(t, "")
	_, err := h.svc.CreateDBInstance(t.Context(), validCreateInput(), testAccountID)
	require.NoError(t, err)

	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	keys, err := bucketKeys(t.Context(), kv)
	require.NoError(t, err)
	require.Contains(t, keys, BootstrapPayloadKey(testDBInstanceID))

	// Nothing anywhere in the account bucket may hold the supplied password.
	for _, key := range keys {
		entry, err := kv.Get(t.Context(), key)
		require.NoError(t, err)
		assert.NotContains(t, string(entry.Value()), "Sup3rSecret!", "cleartext at rest under %s", key)
	}

	var envelope BootstrapPayloadEnvelope
	entry, err := kv.Get(t.Context(), BootstrapPayloadKey(testDBInstanceID))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(entry.Value(), &envelope))
	plaintext, err := handlers_iam.DecryptSecret(envelope.EncryptedPayload, testMasterKey)
	require.NoError(t, err)
	assert.Contains(t, plaintext, "Sup3rSecret!",
		"the payload has to actually carry the password it staged")
}

func TestCreateDBInstance_WithoutAMasterKeyStagesNothing(t *testing.T) {
	h := newCreateHarness(t, "")
	h.svc = h.svc.WithDeps(Deps{
		LoadCA:  newTestCA(t),
		Launch:  h.launch.deps(),
		Network: h.network,
		IAM:     testIAMProvider(h.iam),
	})

	_, err := h.svc.CreateDBInstance(t.Context(), validCreateInput(), testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no master key is configured")

	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	keys, err := bucketKeys(t.Context(), kv)
	require.NoError(t, err)
	assert.NotContains(t, keys, DBInstanceKey(testDBInstanceID), "the reservation is withdrawn")
	assert.NotContains(t, keys, BootstrapPayloadKey(testDBInstanceID))
}

// A restore attaches to a datadir that already has its master role, so nothing
// is staged for it and an acknowledgement against it is denied rather than read
// as a benign duplicate.
func TestRestoredRecord_IsBornWithoutAStagedBootstrap(t *testing.T) {
	svc := newTestService(t)
	rec := defaultRecord()
	rec.Bootstrap.State = BootstrapStateNone
	seedInstance(t, svc, rec)

	out, err := svc.GetDBBootstrapConfig(t.Context(), bootstrapInput(), testAccountID)
	require.NoError(t, err)
	assert.Equal(t, BootstrapModeAttach, out.Mode)
	assert.Nil(t, out.MasterUserPassword)

	stored, _ := readRecord(t, svc)
	assert.Equal(t, BootstrapStateNone, resolveBootstrapState(&stored))
	require.Error(t, acknowledge(t, svc, "bp-anything"))
}

func TestDeleteDBInstance_RemovesTheStagedPayload(t *testing.T) {
	h := newLifecycleHarness(t, false)
	rec := availableRecord()
	seedPending(t, h.svc, rec)
	require.NoError(t, h.svc.PutInstanceIndex(t.Context(), testInstance, InstanceIndexEntry{
		AccountID: testAccountID, DBInstanceIdentifier: testDBID,
	}))

	_, err := h.svc.DeleteDBInstance(t.Context(), skipFinalSnapshot(), testAccountID)
	require.NoError(t, err)

	envelope, _ := storedPayload(t, h.svc, testDBID)
	assert.Nil(t, envelope, "the ciphertext must not outlive the instance it belongs to")
}

// A create that timed out into failed can still be reached by a class change.
// The replacement would lose the format grant only the initial create can hold,
// so it could never come up whatever the password did.
func TestModifyDBInstance_RejectsADisruptiveChangeWhileTheBootstrapIsStaged(t *testing.T) {
	h := newModifyHarness(t)
	rec := modifiableRecord()
	rec.Status = StatusFailed
	seedPending(t, h.svc, rec)

	_, err := h.svc.ModifyDBInstance(t.Context(), &rds.ModifyDBInstanceInput{
		DBInstanceIdentifier: aws.String(testDBID),
		DBInstanceClass:      aws.String("db.t3.large"),
		ApplyImmediately:     aws.Bool(true),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorDBInstanceInvalidState)
	assert.Contains(t, err.Error(), "delete and recreate")

	envelope, _ := storedPayload(t, h.svc, testDBID)
	assert.NotNil(t, envelope, "a rejected modify changes nothing")
}

// The safety net for a generation bump reaching a staged payload through any
// path. The payload is never rebound: rebinding would move where the failure
// surfaces rather than make the replacement work.
//
// Only a VM that never finished its initial bootstrap is a database the customer
// has to recreate. A serving one applied its master role and merely never
// acknowledged, so withdrawing its payload must not read as a failure.
func TestReplaceInstanceVM_DiscardsAStagedBootstrap(t *testing.T) {
	tests := []struct {
		name      string
		status    Status
		wantState string
	}{
		{name: "a create that never bootstrapped", status: StatusFailed,
			wantState: BootstrapStateUnrecoverable},
		{name: "a serving instance that never acknowledged", status: StatusAvailable,
			wantState: BootstrapStatePending},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newModifyHarness(t)
			seed := modifiableRecord()
			seed.Status = tc.status
			rec := seedReplaceable(t, h, seed)
			stagePayload(t, h.svc, &rec)

			require.NoError(t, h.svc.replaceInstanceVM(t.Context(), h.kv(t), testAccountID, &rec,
				replaceInput{Reason: "a class change"}))

			envelope, _ := storedPayload(t, h.svc, testDBID)
			assert.Nil(t, envelope, "a bumped generation must not leave a payload nobody can use")

			after, _ := readRecord(t, h.svc)
			assert.Equal(t, tc.wantState, after.Bootstrap.State)
			if tc.wantState == BootstrapStateUnrecoverable {
				assert.Contains(t, after.Bootstrap.FailureReason, "delete and recreate")
				return
			}
			assert.Empty(t, after.Bootstrap.FailureReason,
				"a serving database must not be reported as one to recreate")
		})
	}
}

// A rollback that could not remove the payload key must not make the identifier
// unusable: the create that follows owns the reservation, so the orphan it finds
// is its own to overwrite.
func TestCreateDBInstance_StagesOverAnOrphanedPayload(t *testing.T) {
	svc := newTestService(t)
	rec := defaultRecord()
	orphaned := seedPending(t, svc, rec)

	restaged := stagePayload(t, svc, &rec)

	assert.NotEqual(t, orphaned, restaged)
	envelope, _ := storedPayload(t, svc, testDBID)
	require.NotNil(t, envelope)
	assert.Equal(t, restaged, envelope.PayloadID, "the live create's payload must be the one a fetch replays")
}

// The reason for an unrecoverable bootstrap is owned by this protocol, not by
// the record's FailureReason: the status machine clears that on every
// transition, which is exactly what a timed-out create does next.
func TestMarkBootstrapUnrecoverable_ReasonSurvivesAStatusTransition(t *testing.T) {
	h := newModifyHarness(t)
	rec := modifiableRecord()
	rec.Status = StatusCreating
	seedInstance(t, h.svc, rec)

	require.NoError(t, h.svc.markBootstrapUnrecoverable(t.Context(), h.kv(t), testAccountID, testDBID,
		"the staged bootstrap payload cannot be read; delete and recreate the DB instance"))
	marked, rev, err := h.svc.getDBInstance(t.Context(), h.kv(t), testDBID)
	require.NoError(t, err)
	require.NoError(t, h.rec.transition(t.Context(), h.kv(t), rev, marked, StatusFailed,
		"the database engine did not report healthy within 15m of creation"))

	stored := h.record(t)
	assert.Contains(t, stored.Bootstrap.FailureReason, "delete and recreate",
		"the only message naming an instance that must be recreated has to outlive the timeout")
	assert.Contains(t, stored.FailureReason, "did not report healthy")
}

// Both reasons reach the customer, and an instance still holding staged
// credentials while it serves is visible without a second call.
func TestProjectDBInstance_ReportsTheBootstrapState(t *testing.T) {
	svc := NewService(nil, testRegion)
	rec := defaultRecord()
	rec.Status = StatusAvailable
	rec.Bootstrap.State = BootstrapStateUnrecoverable
	rec.Bootstrap.FailureReason = "the staged bootstrap payload cannot be read by the control plane"

	infos := svc.projectDBInstance(&rec).StatusInfos

	require.Len(t, infos, 1)
	assert.Equal(t, "bootstrap", aws.StringValue(infos[0].StatusType))
	assert.Equal(t, BootstrapStateUnrecoverable, aws.StringValue(infos[0].Status))
	assert.False(t, aws.BoolValue(infos[0].Normal))
	assert.Equal(t, rec.Bootstrap.FailureReason, aws.StringValue(infos[0].Message))

	rec.Bootstrap.State = BootstrapStatePending
	rec.Bootstrap.FailureReason = ""
	pending := svc.projectDBInstance(&rec).StatusInfos
	require.Len(t, pending, 1, "a serving engine that never confirmed its credentials is reported")
	assert.Equal(t, BootstrapStatePending, aws.StringValue(pending[0].Status))

	rec.Status = StatusCreating
	assert.Empty(t, svc.projectDBInstance(&rec).StatusInfos,
		"a create still in flight is pending by definition and needs no status info")
}

// The gate exists for a create that never bootstrapped, whose replacement VM
// could never format its data volume. A serving instance with a payload staged
// merely never acknowledged, so refusing its class change would be telling the
// customer to destroy a working database.
func TestModifyDBInstance_PendingBootstrapGateIsScopedToFailed(t *testing.T) {
	tests := []struct {
		name    string
		status  Status
		wantErr bool
	}{
		{name: "a create that never bootstrapped", status: StatusFailed, wantErr: true},
		{name: "a serving instance that never acknowledged", status: StatusAvailable},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newModifyHarness(t)
			rec := modifiableRecord()
			rec.Status = tc.status
			seedReplaceable(t, h, rec)
			stagePayload(t, h.svc, &rec)

			in := modifyInput()
			in.DBInstanceClass = aws.String("db.m5.large")
			_, err := h.svc.ModifyDBInstance(t.Context(), in, testAccountID)

			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), awserrors.ErrorDBInstanceInvalidState)
				assert.Contains(t, err.Error(), "delete and recreate")
				return
			}
			require.NoError(t, err)
			envelope, _ := storedPayload(t, h.svc, testDBID)
			assert.NotNil(t, envelope, "an accepted modify must leave the payload replayable")
		})
	}
}
