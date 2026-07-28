package handlers_rds

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"strings"
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

// newTestCA builds a throwaway CA so cert minting is exercised without needing
// the cluster's real ca.key, which the daemon user cannot read.
func newTestCA(t *testing.T) CALoader {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
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
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return func() (*x509.Certificate, *rsa.PrivateKey, error) { return cert, key, nil }
}

func newTestService(t *testing.T) *Service {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	return NewService(nc, testRegion).WithDeps(Deps{LoadCA: newTestCA(t)})
}

// seedInstance writes a DB instance record in the shape CreateDBInstance will
// leave it: bootstrap config pending, marker unset.
func seedInstance(t *testing.T, svc *Service, rec DBInstanceRecord) {
	t.Helper()
	kv, err := svc.bucket(context.Background(), testAccountID)
	require.NoError(t, err)
	require.NoError(t, putJSON(context.Background(), kv, DBInstanceKey(rec.DBInstanceIdentifier), &rec))
}

func defaultRecord() DBInstanceRecord {
	return DBInstanceRecord{
		DBInstanceIdentifier: testDBID,
		AccountID:            testAccountID,
		Status:               StatusCreating,
		Engine:               "postgres",
		EngineVersion:        "18.1",
		DBName:               "orders",
		MasterUsername:       "postgres",
		Port:                 5432,
		InstanceID:           testInstance,
		ENIPrivateIP:         "10.20.30.40",
		DNSName:              "orders-db.123456789012.ap-southeast-2.rds.example.internal",
		Bootstrap: BootstrapState{
			MasterUserPassword: "s3cr3t-master-pw",
			ResolvedParameters: []Parameter{{Name: "shared_buffers", Value: "128MB"}},
		},
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

func TestGetDBBootstrapConfig_InitializeThenAttach(t *testing.T) {
	svc := newTestService(t)
	seedInstance(t, svc, defaultRecord())
	ctx := context.Background()
	in := &GetDBBootstrapConfigInput{DBInstanceIdentifier: testDBID, InstanceID: testInstance}

	first, err := svc.GetDBBootstrapConfig(ctx, in, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, BootstrapModeInitialize, first.Mode)
	require.NotNil(t, first.MasterUserPassword)
	assert.Equal(t, "s3cr3t-master-pw", *first.MasterUserPassword)

	// The rest of the payload has to survive into attach: a fresh VM booting
	// against an existing datadir gets no password but still needs all of it.
	assert.Equal(t, int64(5432), first.Port)
	assert.Equal(t, "orders", first.DBName)
	assert.Equal(t, []Parameter{{Name: "shared_buffers", Value: "128MB"}}, first.Parameters)

	for i := range 3 {
		next, err := svc.GetDBBootstrapConfig(ctx, in, testAccountID)
		require.NoError(t, err, "fetch %d", i)
		assert.Equal(t, BootstrapModeAttach, next.Mode)
		assert.Nil(t, next.MasterUserPassword, "the password must never be re-served")
		assert.Equal(t, int64(5432), next.Port)
		assert.Equal(t, "orders", next.DBName)
		assert.Equal(t, first.Parameters, next.Parameters)
	}
}

func TestGetDBBootstrapConfig_PasswordGoneFromKVAfterConsumption(t *testing.T) {
	svc := newTestService(t)
	seedInstance(t, svc, defaultRecord())

	_, raw := readRecord(t, svc)
	require.Contains(t, raw, "s3cr3t-master-pw", "seed must actually contain the cleartext")

	_, err := svc.GetDBBootstrapConfig(context.Background(),
		&GetDBBootstrapConfigInput{DBInstanceIdentifier: testDBID, InstanceID: testInstance}, testAccountID)
	require.NoError(t, err)

	rec, raw := readRecord(t, svc)
	assert.NotContains(t, raw, "s3cr3t-master-pw", "cleartext must not remain anywhere in the stored record")
	assert.Empty(t, rec.Bootstrap.MasterUserPassword)
	assert.True(t, rec.Bootstrap.Consumed, "only the one-way marker remains")
	assert.NotNil(t, rec.Bootstrap.ConsumedAt)
}

// A restore seeds the marker already flipped, so its very first fetch is an
// attach and no separate restore flag is needed.
func TestGetDBBootstrapConfig_PreConsumedRecordAttachesImmediately(t *testing.T) {
	svc := newTestService(t)
	rec := defaultRecord()
	rec.Bootstrap = BootstrapState{Consumed: true, ResolvedParameters: rec.Bootstrap.ResolvedParameters}
	seedInstance(t, svc, rec)

	out, err := svc.GetDBBootstrapConfig(context.Background(),
		&GetDBBootstrapConfigInput{DBInstanceIdentifier: testDBID, InstanceID: testInstance}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, BootstrapModeAttach, out.Mode)
	assert.Nil(t, out.MasterUserPassword)
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

// TLS is offered, not enforced, so a deployment with no cluster CA must still
// boot a database rather than failing the fetch.
func TestGetDBBootstrapConfig_NoCAStillServesConfig(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	svc := NewService(nc, testRegion)
	seedInstance(t, svc, defaultRecord())

	out, err := svc.GetDBBootstrapConfig(context.Background(),
		&GetDBBootstrapConfigInput{DBInstanceIdentifier: testDBID, InstanceID: testInstance}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, BootstrapModeInitialize, out.Mode)
	assert.Empty(t, out.ServingCertificate)
	assert.Equal(t, int64(5432), out.Port)
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
