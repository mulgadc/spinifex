package handlers_rds

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testDBID     = "orders-db"
	testInstance = "i-0abc123"
)

// Minted once per package rather than per harness. An RSA-2048 keygen costs
// ~140ms under GOFIPS140, and every fixture needs a CA, which dominated the
// package's runtime. The CA is immutable, so sharing it is safe.
var sharedTestCA = sync.OnceValues(func() (*x509.Certificate, *rsa.PrivateKey) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test Cluster CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		panic(err)
	}
	return cert, key
})

// newTestCA builds a throwaway CA so cert minting is exercised without needing
// the cluster's real ca.key, which the daemon user cannot read.
func newTestCA(t *testing.T) CALoader {
	t.Helper()
	cert, key := sharedTestCA()
	return func() (*x509.Certificate, *rsa.PrivateKey, error) { return cert, key, nil }
}

func newTestService(t *testing.T) *Service {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	return NewService(nc, testRegion).WithDeps(Deps{LoadCA: newTestCA(t), MasterKey: testMasterKey})
}

// seedInstance writes a DB instance record without the payload a pending
// bootstrap also needs, which is what an instance past its bootstrap looks like.
// seedPending is the pair for a create that has not been acknowledged yet.
func seedInstance(t *testing.T, svc *Service, rec DBInstanceRecord) {
	t.Helper()
	kv, err := svc.bucket(context.Background(), testAccountID)
	require.NoError(t, err)
	require.NoError(t, putJSON(context.Background(), kv, DBInstanceKey(rec.DBInstanceIdentifier), &rec))
}

func defaultRecord() DBInstanceRecord {
	return DBInstanceRecord{
		DBInstanceIdentifier: testDBID,
		DbiResourceID:        testDbiResourceID,
		AccountID:            testAccountID,
		Status:               StatusCreating,
		Engine:               "postgres",
		EngineVersion:        "18.1",
		DBName:               "orders",
		MasterUsername:       "postgres",
		Port:                 5432,
		InstanceID:           testInstance,
		VMGeneration:         firstVMGeneration,
		ENIPrivateIP:         "10.20.30.40",
		DNSName:              "orders-db.123456789012.ap-southeast-2.rds.example.internal",
		Bootstrap: BootstrapState{
			State:              BootstrapStatePending,
			ResolvedParameters: []Parameter{{Name: "shared_buffers", Value: "128MB"}},
		},
	}
}

// The generation-bound fetch a current VM's agent makes. Cases that only need
// attach material build their own input and leave the generation off.
func bootstrapInput() *GetDBBootstrapConfigInput {
	return &GetDBBootstrapConfigInput{
		DBInstanceIdentifier: testDBID,
		InstanceID:           testInstance,
		VMGeneration:         firstVMGeneration,
	}
}

// readRecord returns the raw stored bytes alongside the decoded record, so a
// test can assert on what is actually at rest in KV rather than on the decode.
func readRecord(t *testing.T, svc *Service) (DBInstanceRecord, string) {
	t.Helper()
	kv, err := svc.bucket(context.Background(), testAccountID)
	require.NoError(t, err)
	entry, err := kv.Get(context.Background(), DBInstanceKey(testDBID))
	require.NoError(t, err)
	var rec DBInstanceRecord
	require.NoError(t, json.Unmarshal(entry.Value(), &rec))
	return rec, string(entry.Value())
}

func TestGetDBBootstrapConfig_ServesTheFullBootMaterial(t *testing.T) {
	svc := newTestService(t)
	rec := defaultRecord()
	rec.DataVolumeID = "vol-data-01"
	rec.DataVolumeSerial = "voldata01"
	seedPending(t, svc, rec)

	out, err := svc.GetDBBootstrapConfig(t.Context(), bootstrapInput(), testAccountID)
	require.NoError(t, err)
	assert.Equal(t, BootstrapModeInitialize, out.Mode)

	// The rest of the payload has to survive into attach: a fresh VM booting
	// against an existing datadir gets no password but still needs all of it.
	assert.Equal(t, int64(5432), out.Port)
	assert.Equal(t, "orders", out.DBName)
	assert.Equal(t, []Parameter{{Name: "shared_buffers", Value: "128MB"}}, out.Parameters)
	assert.Equal(t, "vol-data-01", out.DataVolumeID)
	assert.Equal(t, "voldata01", out.DataVolumeSerial)
	assert.Equal(t, int64(firstVMGeneration), out.VMGeneration)
	assert.False(t, out.FormatAuthorized)
}

func TestGetDBBootstrapConfig_FormatGrantRequiresExactCurrentIdentity(t *testing.T) {
	tests := []struct {
		name       string
		generation int64
		mutate     func(*DBInstanceRecord)
		wantGrant  bool
		wantErr    bool
	}{
		{name: "matching current generation", generation: 1, wantGrant: true},
		{name: "stale generation", generation: 2, wantErr: true},
		{name: "generation omitted by old gateway", wantErr: true},
		{name: "serial does not match volume", generation: 1, mutate: func(rec *DBInstanceRecord) {
			rec.DataVolumeSerial = "volwrong"
		}, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t)
			rec := defaultRecord()
			rec.DataVolumeID = "vol-data-01"
			rec.DataVolumeSerial = "voldata01"
			rec.FormatAuthorized = true
			if tc.mutate != nil {
				tc.mutate(&rec)
			}
			seedPending(t, svc, rec)

			out, err := svc.GetDBBootstrapConfig(t.Context(), &GetDBBootstrapConfigInput{
				DBInstanceIdentifier: testDBID,
				InstanceID:           testInstance,
				VMGeneration:         tc.generation,
			}, testAccountID)
			if tc.wantErr {
				require.Error(t, err)
				envelope, _ := storedPayload(t, svc, testDBID)
				assert.NotNil(t, envelope, "a rejected caller must leave the payload staged for the real one")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantGrant, out.FormatAuthorized)
			assert.Equal(t, rec.DataVolumeID, out.DataVolumeID)
			assert.Equal(t, rec.DataVolumeSerial, out.DataVolumeSerial)
			assert.Equal(t, rec.VMGeneration, out.VMGeneration)
		})
	}
}

func TestGetDBBootstrapConfig_MintsFreshCertEachFetch(t *testing.T) {
	svc := newTestService(t)
	seedInstance(t, svc, defaultRecord())
	ctx := context.Background()
	in := &GetDBBootstrapConfigInput{DBInstanceIdentifier: testDBID, InstanceID: testInstance}

	first, err := svc.GetDBBootstrapConfig(ctx, in, testAccountID)
	require.NoError(t, err)
	second, err := svc.GetDBBootstrapConfig(ctx, in, testAccountID)
	require.NoError(t, err)

	require.NotEmpty(t, first.ServingCertificate)
	require.NotEmpty(t, first.ServingPrivateKey)
	assert.NotEqual(t, first.ServingCertificate, second.ServingCertificate,
		"each fetch must mint rather than return a stored cert")
	assert.NotEqual(t, first.ServingPrivateKey, second.ServingPrivateKey)
	assert.NotEmpty(t, first.CACertificate, "the agent needs the CA to trust its own cert")

	// Neither the cert nor its key may be persisted alongside the record.
	_, raw := readRecord(t, svc)
	assert.NotContains(t, raw, "BEGIN CERTIFICATE")
	assert.NotContains(t, raw, "PRIVATE KEY")
}

// The SAN set is what makes sslmode=verify-full work. The IP is the required
// one, because the endpoint is a bare IP wherever DNS is not configured.
func TestGetDBBootstrapConfig_CertSANsCoverIPAndHostname(t *testing.T) {
	svc := newTestService(t)
	rec := defaultRecord()
	seedInstance(t, svc, rec)

	out, err := svc.GetDBBootstrapConfig(context.Background(),
		&GetDBBootstrapConfigInput{DBInstanceIdentifier: testDBID, InstanceID: testInstance}, testAccountID)
	require.NoError(t, err)

	cert := parseCertPEM(t, out.ServingCertificate)
	require.Len(t, cert.IPAddresses, 1)
	assert.True(t, cert.IPAddresses[0].Equal(net.ParseIP(rec.ENIPrivateIP)))
	assert.Equal(t, []string{rec.DNSName}, cert.DNSNames)
	assert.Equal(t, testDBID, cert.Subject.CommonName)
}

func TestGetDBBootstrapConfig_NoDNSNameStillMintsIPOnlyCert(t *testing.T) {
	svc := newTestService(t)
	rec := defaultRecord()
	rec.DNSName = ""
	seedInstance(t, svc, rec)

	out, err := svc.GetDBBootstrapConfig(context.Background(),
		&GetDBBootstrapConfigInput{DBInstanceIdentifier: testDBID, InstanceID: testInstance}, testAccountID)
	require.NoError(t, err)

	cert := parseCertPEM(t, out.ServingCertificate)
	assert.Empty(t, cert.DNSNames)
	require.Len(t, cert.IPAddresses, 1)
}

// A deployment with no cluster CA cannot enforce TLS, and a set that requires it
// is refused at binding rather than here — so the fetch must still serve a
// config and boot a database.
func TestGetDBBootstrapConfig_NoCAStillServesConfig(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	svc := NewService(nc, testRegion).WithDeps(Deps{MasterKey: testMasterKey})
	seedPending(t, svc, defaultRecord())

	out, err := svc.GetDBBootstrapConfig(t.Context(), bootstrapInput(), testAccountID)
	require.NoError(t, err)
	assert.Equal(t, BootstrapModeInitialize, out.Mode)
	assert.Empty(t, out.ServingCertificate)
	assert.Equal(t, int64(5432), out.Port)
}

func TestGetDBBootstrapConfig_ConfiguredCARequiresENIPrivateIP(t *testing.T) {
	svc := newTestService(t)
	rec := defaultRecord()
	rec.ENIPrivateIP = ""
	seedPending(t, svc, rec)

	_, err := svc.GetDBBootstrapConfig(t.Context(), bootstrapInput(), testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "configured TLS requires an ENI private IP")

	envelope, _ := storedPayload(t, svc, testDBID)
	assert.NotNil(t, envelope, "a failed fetch must leave the payload replayable")
}

func TestGetDBBootstrapConfig_PartialCAConfigurationFailsClosed(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	svc := NewService(nc, testRegion).WithDeps(Deps{
		CACertPath: "/etc/spinifex/ca.pem",
		MasterKey:  testMasterKey,
	})
	seedPending(t, svc, defaultRecord())

	_, err := svc.GetDBBootstrapConfig(t.Context(), bootstrapInput(), testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "incomplete cluster CA configuration")

	envelope, _ := storedPayload(t, svc, testDBID)
	assert.NotNil(t, envelope)
}

// A configured CA that cannot be loaded fails the fetch before the payload is
// ever read. The staged password is untouched, which is what makes the agent's
// retry succeed once the CA is readable again.
func TestGetDBBootstrapConfig_UnloadableCALeavesPasswordRecoverable(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	loadErr := errors.New("read CA key /etc/spinifex/ca.key: permission denied")
	broken := true
	svc := NewService(nc, testRegion).WithDeps(Deps{
		MasterKey: testMasterKey,
		LoadCA: func() (*x509.Certificate, *rsa.PrivateKey, error) {
			if broken {
				return nil, nil, loadErr
			}
			return newTestCA(t)()
		},
	})
	seedPending(t, svc, defaultRecord())

	_, err := svc.GetDBBootstrapConfig(t.Context(), bootstrapInput(), testAccountID)
	require.Error(t, err)
	envelope, _ := storedPayload(t, svc, testDBID)
	require.NotNil(t, envelope, "the password must still be staged to retry against")

	// Once the CA is readable the retry bootstraps normally.
	broken = false
	out, err := svc.GetDBBootstrapConfig(t.Context(), bootstrapInput(), testAccountID)
	require.NoError(t, err)
	assert.Equal(t, BootstrapModeInitialize, out.Mode)
	require.NotNil(t, out.MasterUserPassword)
	assert.Equal(t, testMasterPassword, *out.MasterUserPassword)
	assert.NotEmpty(t, out.ServingCertificate)
}

// Under consume-on-fetch, concurrent fetches racing for a one-shot password was
// the whole problem. A replay has no race to lose: every caller bound to the
// generation is served the same password and none of them writes.
func TestGetDBBootstrapConfig_ConcurrentFetchesAllReplayTheSamePassword(t *testing.T) {
	svc := newTestService(t)
	payloadID := seedPending(t, svc, defaultRecord())
	_, beforeRaw := readRecord(t, svc)

	const fetches = 4
	outs := make([]*GetDBBootstrapConfigOutput, fetches)
	errs := make([]error, fetches)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range fetches {
		wg.Go(func() {
			<-start
			outs[i], errs[i] = svc.GetDBBootstrapConfig(context.Background(), bootstrapInput(), testAccountID)
		})
	}
	close(start)
	wg.Wait()

	for i := range fetches {
		require.NoError(t, errs[i])
		require.NotNil(t, outs[i].MasterUserPassword, "fetch %d", i)
		assert.Equal(t, testMasterPassword, *outs[i].MasterUserPassword)
		assert.Equal(t, payloadID, outs[i].PayloadID)
		// Serving material is minted per call, so no caller boots with ssl=off
		// while another got TLS.
		assert.NotEmpty(t, outs[i].ServingCertificate)
		assert.NotEmpty(t, outs[i].ServingPrivateKey)
	}

	_, afterRaw := readRecord(t, svc)
	assert.Equal(t, beforeRaw, afterRaw)
}

func TestGetDBBootstrapConfig_UnknownInstance(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.GetDBBootstrapConfig(context.Background(),
		&GetDBBootstrapConfigInput{DBInstanceIdentifier: "ghost", InstanceID: testInstance}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorDBInstanceNotFound, err.Error())
}

func parseCertPEM(t *testing.T, pemStr string) *x509.Certificate {
	t.Helper()
	require.True(t, strings.HasPrefix(pemStr, "-----BEGIN CERTIFICATE-----"))
	block, _ := pem.Decode([]byte(pemStr))
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	return cert
}

func TestRegisterDBInstance_IdempotentAndRefreshesLastSeen(t *testing.T) {
	svc := newTestService(t)
	seedInstance(t, svc, defaultRecord())
	ctx := context.Background()
	in := &RegisterDBInstanceInput{
		DBInstanceIdentifier: testDBID, InstanceID: testInstance,
		AgentVersion: "1.0.0", EngineVersion: "18.1",
	}

	out, err := svc.RegisterDBInstance(ctx, in, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, testDBID, out.DBInstanceIdentifier)
	assert.Equal(t, int64(30), out.HeartbeatIntervalSeconds)

	first, _ := readRecord(t, svc)
	require.NotNil(t, first.Agent.RegisteredAt)
	require.NotNil(t, first.Agent.LastSeen)
	assert.Equal(t, "1.0.0", first.Agent.AgentVersion)

	time.Sleep(2 * time.Millisecond)
	_, err = svc.RegisterDBInstance(ctx, in, testAccountID)
	require.NoError(t, err)

	second, _ := readRecord(t, svc)
	assert.Equal(t, *first.Agent.RegisteredAt, *second.Agent.RegisteredAt,
		"re-registering the same VM keeps its original registration time")
	assert.True(t, second.Agent.LastSeen.After(*first.Agent.LastSeen), "liveness must be refreshed")
}

// A replace mints a new instance ID, which is a new registration rather than a
// continuation of the old VM's.
func TestRegisterDBInstance_NewVMResetsRegisteredAt(t *testing.T) {
	svc := newTestService(t)
	seedInstance(t, svc, defaultRecord())
	ctx := context.Background()

	_, err := svc.RegisterDBInstance(ctx, &RegisterDBInstanceInput{DBInstanceIdentifier: testDBID, InstanceID: testInstance}, testAccountID)
	require.NoError(t, err)
	first, _ := readRecord(t, svc)

	time.Sleep(2 * time.Millisecond)
	_, err = svc.RegisterDBInstance(ctx, &RegisterDBInstanceInput{DBInstanceIdentifier: testDBID, InstanceID: "i-0replaced"}, testAccountID)
	require.NoError(t, err)

	second, _ := readRecord(t, svc)
	assert.Equal(t, "i-0replaced", second.Agent.InstanceID)
	assert.True(t, second.Agent.RegisteredAt.After(*first.Agent.RegisteredAt))
}

func TestRegisterDBInstance_UnknownInstance(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.RegisterDBInstance(context.Background(),
		&RegisterDBInstanceInput{DBInstanceIdentifier: "ghost", InstanceID: testInstance}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorDBInstanceNotFound, err.Error())
}

// Liveness is the hottest path in the service, so an unchanged beat must stay in
// memory and only reach KV on the slower floor.
func TestSubmitDBStateChange_PersistsOnChangeAndOnFloor(t *testing.T) {
	svc := newTestService(t)
	seedInstance(t, svc, defaultRecord())
	ctx := context.Background()
	beat := func(h EngineHealth, msg string) *SubmitDBStateChangeOutput {
		t.Helper()
		out, err := svc.SubmitDBStateChange(ctx, &SubmitDBStateChangeInput{
			DBInstanceIdentifier: testDBID, InstanceID: testInstance, EngineHealth: h, Message: msg,
		}, testAccountID)
		require.NoError(t, err)
		return out
	}

	// First beat is a change (nothing seen before), so it persists.
	assert.True(t, beat(EngineHealthStarting, "").Persisted)

	// Steady beats stay in memory until the floor is reached.
	for i := range HeartbeatPersistEvery - 1 {
		assert.False(t, beat(EngineHealthStarting, "").Persisted, "beat %d must not reach KV", i)
	}
	assert.True(t, beat(EngineHealthStarting, "").Persisted, "the floor forces a persist")

	// A changed health persists immediately, without waiting for the floor.
	assert.True(t, beat(EngineHealthHealthy, "").Persisted)
	assert.False(t, beat(EngineHealthHealthy, "").Persisted)
	// So does a changed message on an unchanged health — that is the failure
	// reason the reconciler reports.
	assert.True(t, beat(EngineHealthHealthy, "replication lag").Persisted)

	rec, _ := readRecord(t, svc)
	assert.Equal(t, EngineHealthHealthy, rec.Agent.EngineHealth)
	assert.Equal(t, "replication lag", rec.Agent.Message)
	assert.NotNil(t, rec.Agent.LastSeen)
}

func TestSubmitDBStateChange_RecordsParameterRollback(t *testing.T) {
	svc := newTestService(t)
	rec := defaultRecord()
	rec.DBParameterGroupName = "customer-params"
	seedInstance(t, svc, rec)

	out, err := svc.SubmitDBStateChange(t.Context(), &SubmitDBStateChangeInput{
		DBInstanceIdentifier: testDBID,
		InstanceID:           testInstance,
		EngineHealth:         EngineHealthUnhealthy,
		Message:              ParameterRollbackMessage,
	}, testAccountID)
	require.NoError(t, err)
	assert.True(t, out.Persisted)

	stored, _ := readRecord(t, svc)
	assert.True(t, stored.ParametersRolledBack)
	groups := projectParameterGroup(&stored)
	require.Len(t, groups, 1)
	assert.Equal(t, "failed-to-apply", *groups[0].ParameterApplyStatus)

	kv, err := svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	var ring eventRing
	found, err := getJSON(t.Context(), kv, EventRingKey(EventSourceTypeDBInstance, testDBID), &ring)
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, ring.Events, 1)
	assert.Contains(t, ring.Events[0].Categories, EventCategoryConfigurationChange)
	assert.Contains(t, ring.Events[0].Categories, EventCategoryFailure)
}

// In-memory liveness is fresher than the record between persists, which is what
// lets the reconciler judge staleness without reading KV every tick.
func TestSubmitDBStateChange_LastSeenTracksUnpersistedBeats(t *testing.T) {
	svc := newTestService(t)
	seedInstance(t, svc, defaultRecord())
	ctx := context.Background()

	_, err := svc.SubmitDBStateChange(ctx, &SubmitDBStateChangeInput{
		DBInstanceIdentifier: testDBID, InstanceID: testInstance, EngineHealth: EngineHealthHealthy,
	}, testAccountID)
	require.NoError(t, err)
	persisted, _ := readRecord(t, svc)

	time.Sleep(2 * time.Millisecond)
	out, err := svc.SubmitDBStateChange(ctx, &SubmitDBStateChangeInput{
		DBInstanceIdentifier: testDBID, InstanceID: testInstance, EngineHealth: EngineHealthHealthy,
	}, testAccountID)
	require.NoError(t, err)
	require.False(t, out.Persisted)

	seen, ok := svc.LastSeen(testAccountID, testDBID)
	require.True(t, ok)
	assert.True(t, seen.After(*persisted.Agent.LastSeen))

	// A node that has seen no beat falls back to the record rather than
	// reporting a bogus zero time.
	_, ok = svc.LastSeen(testAccountID, "other-db")
	assert.False(t, ok)
}

func TestSubmitDBStateChange_RejectsUnknownHealth(t *testing.T) {
	svc := newTestService(t)
	seedInstance(t, svc, defaultRecord())
	_, err := svc.SubmitDBStateChange(context.Background(), &SubmitDBStateChangeInput{
		DBInstanceIdentifier: testDBID, InstanceID: testInstance, EngineHealth: "on-fire",
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
}

func TestInstanceIndex_RoundTrip(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	missing, err := svc.LookupInstanceIndex(ctx, testInstance)
	require.NoError(t, err)
	assert.Nil(t, missing, "a non-RDS instance resolves to nothing rather than an error")

	require.NoError(t, svc.PutInstanceIndex(ctx, testInstance, InstanceIndexEntry{
		AccountID: testAccountID, DBInstanceIdentifier: testDBID, VMGeneration: 1,
	}))

	entry, err := svc.LookupInstanceIndex(ctx, testInstance)
	require.NoError(t, err)
	require.NotNil(t, entry)
	assert.Equal(t, testAccountID, entry.AccountID)
	assert.Equal(t, testDBID, entry.DBInstanceIdentifier)

	require.NoError(t, svc.DeleteInstanceIndex(ctx, testInstance))
	gone, err := svc.LookupInstanceIndex(ctx, testInstance)
	require.NoError(t, err)
	assert.Nil(t, gone)
}
