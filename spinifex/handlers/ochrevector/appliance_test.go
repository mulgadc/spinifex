// Exercises unexported ochrevector appliance internals with no exported
// surface to drive them through.
//
//test:in-package
package handlers_ochrevector

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLauncher is the test double for ApplianceLauncher: it counts
// invocations and records every identifier/password it was called with, so
// tests can assert exactly-once launch and password reuse without any real
// VM.
type fakeLauncher struct {
	mu          sync.Mutex
	calls       int
	identifiers []string
	passwords   []string
	endpoint    string
	port        int
	err         error

	deleteCalls []string
	deleteErr   error
}

var _ ApplianceLauncher = (*fakeLauncher)(nil)

func (f *fakeLauncher) Launch(_ context.Context, identifier, _ string, masterPassword string) (string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.identifiers = append(f.identifiers, identifier)
	f.passwords = append(f.passwords, masterPassword)
	if f.err != nil {
		return "", 0, f.err
	}
	return f.endpoint, f.port, nil
}

func (f *fakeLauncher) Delete(_ context.Context, identifier string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls = append(f.deleteCalls, identifier)
	return f.deleteErr
}

func (f *fakeLauncher) deleteCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.deleteCalls)
}

func (f *fakeLauncher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeLauncher) lastPassword() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.passwords) == 0 {
		return ""
	}
	return f.passwords[len(f.passwords)-1]
}

func testMasterKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	return key
}

func TestNewAppliance_RequiresMasterKey(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	_, err := NewAppliance(js, nil, &fakeLauncher{})
	assert.Error(t, err)
}

func TestNewAppliance_RequiresLauncher(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	_, err := NewAppliance(js, testMasterKey(t), nil)
	assert.Error(t, err)
}

// TestCredentialRoundTrip proves the generated master password survives
// encrypt-then-decrypt unchanged, and that the persisted ciphertext is never
// the plaintext itself.
func TestCredentialRoundTrip(t *testing.T) {
	masterKey := testMasterKey(t)

	password, err := generateMasterPassword()
	require.NoError(t, err)
	assert.Len(t, password, appliancePasswordLength)

	encrypted, err := handlers_iam.EncryptSecret(password, masterKey)
	require.NoError(t, err)
	assert.NotEqual(t, password, encrypted)
	assert.NotContains(t, encrypted, password)

	rec := ApplianceRecord{EncryptedPassword: encrypted}
	appliance := &Appliance{masterKey: masterKey}
	decrypted, err := appliance.decryptPassword(&rec)
	require.NoError(t, err)
	assert.Equal(t, password, decrypted)

	data, err := json.Marshal(rec)
	require.NoError(t, err)
	assert.NotContains(t, string(data), password)
}

// TestEnsure_SingletonRace is the singleton-claim proof: N concurrent Ensure
// calls against one Appliance must produce exactly one launcher invocation
// and hand every caller back the same connection info.
func TestEnsure_SingletonRace(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	launcher := &fakeLauncher{endpoint: "10.0.0.5", port: 5432}
	appliance, err := NewAppliance(js, testMasterKey(t), launcher)
	require.NoError(t, err)

	const n = 5
	type result struct {
		info ApplianceConnInfo
		err  error
	}
	results := make(chan result, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			info, err := appliance.Ensure(context.Background())
			results <- result{info: info, err: err}
		})
	}
	wg.Wait()
	close(results)

	var infos []ApplianceConnInfo
	for r := range results {
		require.NoError(t, r.err)
		infos = append(infos, r.info)
	}
	require.Len(t, infos, n)
	for _, info := range infos {
		assert.Equal(t, infos[0], info)
	}
	assert.Equal(t, "10.0.0.5", infos[0].Endpoint)
	assert.Equal(t, 1, launcher.callCount())
}

// TestEnsure_LoserReturnsAvailableWithoutRelaunch proves a second, later
// Ensure call against an already-AVAILABLE appliance returns immediately
// with the same connection info and never calls the launcher again.
func TestEnsure_LoserReturnsAvailableWithoutRelaunch(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	launcher := &fakeLauncher{endpoint: "10.0.0.6", port: 5432}
	appliance, err := NewAppliance(js, testMasterKey(t), launcher)
	require.NoError(t, err)

	first, err := appliance.Ensure(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, launcher.callCount())

	second, err := appliance.Ensure(context.Background())
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.Equal(t, 1, launcher.callCount())
}

// TestEnsure_StaleProvisioningIsResumed proves a PROVISIONING record left
// behind by a crashed winner (UpdatedAt older than applianceStaleAfter) is
// re-driven through the SAME identifier and stored password, reaching
// AVAILABLE, rather than a second claim being minted.
func TestEnsure_StaleProvisioningIsResumed(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	masterKey := testMasterKey(t)
	launcher := &fakeLauncher{endpoint: "10.0.0.7", port: 5432}
	appliance, err := NewAppliance(js, masterKey, launcher)
	require.NoError(t, err)

	seedPassword, err := generateMasterPassword()
	require.NoError(t, err)
	encrypted, err := handlers_iam.EncryptSecret(seedPassword, masterKey)
	require.NoError(t, err)

	stale := time.Now().UTC().Add(-2 * applianceStaleAfter)
	seed := ApplianceRecord{
		Identifier:        ApplianceIdentifier,
		MasterUsername:    applianceMasterUsername,
		EncryptedPassword: encrypted,
		State:             ApplianceStateProvisioning,
		CreatedAt:         stale,
		UpdatedAt:         stale,
	}
	data, err := json.Marshal(seed)
	require.NoError(t, err)

	ctx := context.Background()
	kv := applianceRawKV(t, appliance)
	_, err = kv.Create(ctx, appliancePostgresKey, data)
	require.NoError(t, err)

	info, err := appliance.Ensure(ctx)
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.7", info.Endpoint)
	assert.Equal(t, 1, launcher.callCount())
	assert.Equal(t, ApplianceIdentifier, launcher.identifiers[0])
	assert.Equal(t, seedPassword, launcher.lastPassword())
}

// TestEnsure_FreshProvisioningIsWaitedNotResumed proves a PROVISIONING
// record within the staleness grace period is waited on, bounded by the
// caller's context, and never resumed/relaunched.
func TestEnsure_FreshProvisioningIsWaitedNotResumed(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	masterKey := testMasterKey(t)
	launcher := &fakeLauncher{endpoint: "10.0.0.8", port: 5432}
	appliance, err := NewAppliance(js, masterKey, launcher)
	require.NoError(t, err)

	seedPassword, err := generateMasterPassword()
	require.NoError(t, err)
	encrypted, err := handlers_iam.EncryptSecret(seedPassword, masterKey)
	require.NoError(t, err)

	fresh := time.Now().UTC()
	seed := ApplianceRecord{
		Identifier:        ApplianceIdentifier,
		MasterUsername:    applianceMasterUsername,
		EncryptedPassword: encrypted,
		State:             ApplianceStateProvisioning,
		CreatedAt:         fresh,
		UpdatedAt:         fresh,
	}
	data, err := json.Marshal(seed)
	require.NoError(t, err)

	ctx := context.Background()
	kv := applianceRawKV(t, appliance)
	_, err = kv.Create(ctx, appliancePostgresKey, data)
	require.NoError(t, err)

	waitCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	_, err = appliance.Ensure(waitCtx)
	assert.Error(t, err)
	assert.Equal(t, 0, launcher.callCount())
}

// TestEnsure_PlaintextPasswordNeverPersisted proves the plaintext master
// password the launcher receives is never present in the raw bytes stored in
// the KV bucket.
func TestEnsure_PlaintextPasswordNeverPersisted(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	launcher := &fakeLauncher{endpoint: "10.0.0.9", port: 5432}
	appliance, err := NewAppliance(js, testMasterKey(t), launcher)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = appliance.Ensure(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, launcher.callCount())

	password := launcher.lastPassword()
	require.NotEmpty(t, password)

	kv := applianceRawKV(t, appliance)
	entry, err := kv.Get(ctx, appliancePostgresKey)
	require.NoError(t, err)
	assert.NotContains(t, string(entry.Value()), password)
}

// TestEnsure_LaunchFailureLeavesRecordProvisioning proves a winner whose
// launch fails leaves the record PROVISIONING rather than rolling it back,
// and that a second, still-fresh Ensure call neither relaunches nor mints a
// new password for it.
func TestEnsure_LaunchFailureLeavesRecordProvisioning(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	masterKey := testMasterKey(t)
	launcher := &fakeLauncher{err: errors.New("boom")}
	appliance, err := NewAppliance(js, masterKey, launcher)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = appliance.Ensure(ctx)
	assert.Error(t, err)
	require.Equal(t, 1, launcher.callCount())
	firstPassword := launcher.lastPassword()

	rec, _, err := appliance.getRecord(ctx)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, ApplianceStateProvisioning, rec.State)

	waitCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	_, err = appliance.Ensure(waitCtx)
	assert.Error(t, err)
	assert.Equal(t, 1, launcher.callCount())
	assert.Equal(t, firstPassword, launcher.lastPassword())
}

// TestResume_LaunchFailureLeavesRecordProvisioning proves a re-driven launch
// of a stale record that also fails leaves the record PROVISIONING with the
// same stored password, not a rollback or a fresh claim.
func TestResume_LaunchFailureLeavesRecordProvisioning(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	masterKey := testMasterKey(t)
	launcher := &fakeLauncher{err: errors.New("boom")}
	appliance, err := NewAppliance(js, masterKey, launcher)
	require.NoError(t, err)

	seedPassword, err := generateMasterPassword()
	require.NoError(t, err)
	encrypted, err := handlers_iam.EncryptSecret(seedPassword, masterKey)
	require.NoError(t, err)

	stale := time.Now().UTC().Add(-2 * applianceStaleAfter)
	seed := ApplianceRecord{
		Identifier:        ApplianceIdentifier,
		MasterUsername:    applianceMasterUsername,
		EncryptedPassword: encrypted,
		State:             ApplianceStateProvisioning,
		CreatedAt:         stale,
		UpdatedAt:         stale,
	}
	data, err := json.Marshal(seed)
	require.NoError(t, err)

	ctx := context.Background()
	kv := applianceRawKV(t, appliance)
	_, err = kv.Create(ctx, appliancePostgresKey, data)
	require.NoError(t, err)

	_, err = appliance.Ensure(ctx)
	assert.Error(t, err)
	assert.Equal(t, 1, launcher.callCount())
	assert.Equal(t, seedPassword, launcher.lastPassword())

	rec, _, err := appliance.getRecord(ctx)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, ApplianceStateProvisioning, rec.State)
}

// TestConnect_NotProvisioned proves Connect refuses to build a backend when
// the appliance has no record yet, without needing any postgres to run.
func TestConnect_NotProvisioned(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	appliance, err := NewAppliance(js, testMasterKey(t), &fakeLauncher{})
	require.NoError(t, err)

	_, err = appliance.Connect(context.Background())
	assert.Error(t, err)
}

// TestConnect_NotAvailable proves Connect refuses a PROVISIONING record.
func TestConnect_NotAvailable(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	masterKey := testMasterKey(t)
	appliance, err := NewAppliance(js, masterKey, &fakeLauncher{})
	require.NoError(t, err)

	password, err := generateMasterPassword()
	require.NoError(t, err)
	encrypted, err := handlers_iam.EncryptSecret(password, masterKey)
	require.NoError(t, err)

	now := time.Now().UTC()
	seed := ApplianceRecord{
		Identifier:        ApplianceIdentifier,
		MasterUsername:    applianceMasterUsername,
		EncryptedPassword: encrypted,
		State:             ApplianceStateProvisioning,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	data, err := json.Marshal(seed)
	require.NoError(t, err)

	ctx := context.Background()
	kv := applianceRawKV(t, appliance)
	_, err = kv.Create(ctx, appliancePostgresKey, data)
	require.NoError(t, err)

	_, err = appliance.Connect(ctx)
	assert.Error(t, err)
}

// applianceRawKV returns the appliance's underlying bucket, for the tests that
// seed or inspect the stored bytes directly rather than through the record API.
func applianceRawKV(t *testing.T, appliance *Appliance) jetstream.KeyValue {
	t.Helper()
	kv, err := appliance.store.KV(t.Context())
	require.NoError(t, err)
	return kv
}

// seedAvailableAppliance seeds an AVAILABLE record so Connect gets past its
// state checks and reaches the host-port/dial path.
func seedAvailableAppliance(t *testing.T, appliance *Appliance, masterKey []byte, endpoint string, port int) {
	t.Helper()
	password, err := generateMasterPassword()
	require.NoError(t, err)
	encrypted, err := handlers_iam.EncryptSecret(password, masterKey)
	require.NoError(t, err)

	now := time.Now().UTC()
	seed := ApplianceRecord{
		Identifier:        ApplianceIdentifier,
		MasterUsername:    applianceMasterUsername,
		EncryptedPassword: encrypted,
		Endpoint:          endpoint,
		Port:              port,
		State:             ApplianceStateAvailable,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	data, err := json.Marshal(seed)
	require.NoError(t, err)

	ctx := context.Background()
	kv := applianceRawKV(t, appliance)
	_, err = kv.Create(ctx, appliancePostgresKey, data)
	require.NoError(t, err)
}

// TestConnect_NoHostPortDepsSkipsEnsure proves an Appliance that never calls
// WithHostPort performs no host-port work: Connect fails on the (unreachable
// in this test) pgx dial, not on anything host-port related, and the ENI
// side is left untouched.
func TestConnect_NoHostPortDepsSkipsEnsure(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	masterKey := testMasterKey(t)
	appliance, err := NewAppliance(js, masterKey, &fakeLauncher{})
	require.NoError(t, err)
	seedAvailableAppliance(t, appliance, masterKey, "127.0.0.1", 1)

	_, err = appliance.Connect(context.Background())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "ensure daemon host port")
}

// TestConnect_EnsuresHostPortBeforeDialing_PropagatesFailure proves Connect
// drives the daemon's VPC host port before it ever builds a DSN or dials
// Postgres: a host-port failure surfaces as that failure, not as a pgx
// connection error, and the ENI is still minted along the way.
func TestConnect_EnsuresHostPortBeforeDialing_PropagatesFailure(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	masterKey := testMasterKey(t)
	appliance, err := NewAppliance(js, masterKey, &fakeLauncher{})
	require.NoError(t, err)
	seedAvailableAppliance(t, appliance, masterKey, testApplianceEndpoint, 5432)

	h := newHostPortHarness()
	h.putSubnet(testApplianceSubnet, "10.244.1.0/24")
	h.putApplianceENI(ApplianceIdentifier, testApplianceSubnet, testApplianceEndpoint)
	h.hostPort.failEnsure = errors.New("ovs-vsctl exploded")
	appliance.WithHostPort(h.deps())

	_, err = appliance.Connect(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ensure daemon host port")
	assert.Contains(t, err.Error(), "ovs-vsctl exploded")
	assert.Len(t, h.vpc.created, 1, "the ENI is minted before the host port install that then fails")
}

// TestAppliance_TeardownHostPort proves TeardownHostPort removes the port
// Connect last installed, and is a no-op both before any Connect and for an
// Appliance that never opted into host-port management at all.
func TestAppliance_TeardownHostPort(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	appliance, err := NewAppliance(js, testMasterKey(t), &fakeLauncher{})
	require.NoError(t, err)

	assert.NoError(t, appliance.TeardownHostPort(), "never having connected must be a harmless no-op")

	h := newHostPortHarness()
	appliance.WithHostPort(h.deps())
	assert.NoError(t, appliance.TeardownHostPort(), "no ENI ensured yet: still a no-op")
	assert.Empty(t, h.hostPort.removals())

	appliance.mu.Lock()
	appliance.eniID = "eni-installed"
	appliance.mu.Unlock()

	require.NoError(t, appliance.TeardownHostPort())
	assert.Equal(t, []string{"eni-installed"}, h.hostPort.removals())
}

// TestTeardown_DeletesRecordLauncherAndHostPort proves Teardown drives all
// three steps: the launcher's Delete for the fixed appliance identifier, the
// daemon's own host port removal, and the KV record.
func TestTeardown_DeletesRecordLauncherAndHostPort(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	masterKey := testMasterKey(t)
	launcher := &fakeLauncher{endpoint: "10.0.0.30", port: 5432}
	appliance, err := NewAppliance(js, masterKey, launcher)
	require.NoError(t, err)
	seedAvailableAppliance(t, appliance, masterKey, "10.0.0.30", 5432)

	h := newHostPortHarness()
	appliance.WithHostPort(h.deps())
	appliance.mu.Lock()
	appliance.eniID = "eni-installed"
	appliance.mu.Unlock()

	require.NoError(t, appliance.Teardown(context.Background(), false))

	assert.Equal(t, []string{ApplianceIdentifier}, launcher.deleteCalls)
	assert.Equal(t, []string{"eni-installed"}, h.hostPort.removals())

	rec, _, err := appliance.getRecord(context.Background())
	require.NoError(t, err)
	assert.Nil(t, rec, "the KV record must be gone after teardown")
}

// TestTeardown_NoExistingApplianceIsANoOp proves tearing down an appliance
// that was never provisioned (or already torn down) succeeds rather than
// erroring on an absent KV record or an already-gone RDS instance.
func TestTeardown_NoExistingApplianceIsANoOp(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	launcher := &fakeLauncher{}
	appliance, err := NewAppliance(js, testMasterKey(t), launcher)
	require.NoError(t, err)

	assert.NoError(t, appliance.Teardown(context.Background(), false))
	assert.Equal(t, 1, launcher.deleteCallCount())

	// Tearing down again must still be a no-op success.
	assert.NoError(t, appliance.Teardown(context.Background(), false))
}

// TestTeardown_StillDeletesRecordWhenHostPortRemovalFails proves a host-port
// removal failure is best-effort: it does not stop the KV record from being
// deleted, but it is still surfaced in the joined error rather than swallowed.
func TestTeardown_StillDeletesRecordWhenHostPortRemovalFails(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	masterKey := testMasterKey(t)
	launcher := &fakeLauncher{endpoint: "10.0.0.31", port: 5432}
	appliance, err := NewAppliance(js, masterKey, launcher)
	require.NoError(t, err)
	seedAvailableAppliance(t, appliance, masterKey, "10.0.0.31", 5432)

	h := newHostPortHarness()
	h.hostPort.failRemove = errors.New("ovs-vsctl exploded")
	appliance.WithHostPort(h.deps())
	appliance.mu.Lock()
	appliance.eniID = "eni-installed"
	appliance.mu.Unlock()

	err = appliance.Teardown(context.Background(), false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ovs-vsctl exploded")

	rec, _, err := appliance.getRecord(context.Background())
	require.NoError(t, err)
	assert.Nil(t, rec, "the KV record must still be deleted despite the host-port failure")
}

// TestTeardown_JoinsLauncherAndRecordErrors proves a launcher.Delete failure
// does not stop the KV record from being deleted, and both failures are
// joined into the returned error.
func TestTeardown_JoinsLauncherAndRecordErrors(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	masterKey := testMasterKey(t)
	launcher := &fakeLauncher{deleteErr: errors.New("rds unreachable")}
	appliance, err := NewAppliance(js, masterKey, launcher)
	require.NoError(t, err)
	seedAvailableAppliance(t, appliance, masterKey, "10.0.0.32", 5432)

	err = appliance.Teardown(context.Background(), false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rds unreachable")

	rec, _, err := appliance.getRecord(context.Background())
	require.NoError(t, err)
	assert.Nil(t, rec, "the KV record must still be deleted despite the launcher failure")
}

// TestTeardown_DefaultPreservesRegistryButPurgesJobs proves a default
// Teardown (purgeMetadata=false) leaves the index registry intact -- so a
// later Ensure can reconcile from it -- while still clearing job history,
// which is scoped to the now-destroyed RDS instance.
func TestTeardown_DefaultPreservesRegistryButPurgesJobs(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	masterKey := testMasterKey(t)
	launcher := &fakeLauncher{endpoint: "10.0.0.33", port: 5432}
	appliance, err := NewAppliance(js, masterKey, launcher)
	require.NoError(t, err)
	seedAvailableAppliance(t, appliance, masterKey, "10.0.0.33", 5432)

	registry := NewRegistry(js)
	jobs := NewJobStore(js)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, registry.Reserve(ctx, "111111111111", Record{ID: "idx-one", CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, jobs.Reserve(ctx, "111111111111", JobRecord{ID: "job-one", CreatedAt: now, UpdatedAt: now}))

	appliance.WithStores(registry, jobs)
	require.NoError(t, appliance.Teardown(ctx, false))

	remainingIndexes, err := registry.ListAll(ctx)
	require.NoError(t, err)
	assert.Len(t, remainingIndexes, 1, "default Teardown must leave the index registry intact")

	remainingJobs, err := jobs.ListAll(ctx)
	require.NoError(t, err)
	assert.Empty(t, remainingJobs, "Teardown must still purge job history")
}

// TestTeardown_PurgeMetadataPurgesRegistryAndJobs proves purgeMetadata=true
// restores the old full wipe of the index registry (and job history)
// alongside the RDS instance, for an intentional full destroy.
func TestTeardown_PurgeMetadataPurgesRegistryAndJobs(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	masterKey := testMasterKey(t)
	launcher := &fakeLauncher{endpoint: "10.0.0.36", port: 5432}
	appliance, err := NewAppliance(js, masterKey, launcher)
	require.NoError(t, err)
	seedAvailableAppliance(t, appliance, masterKey, "10.0.0.36", 5432)

	registry := NewRegistry(js)
	jobs := NewJobStore(js)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, registry.Reserve(ctx, "111111111111", Record{ID: "idx-one", CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, jobs.Reserve(ctx, "111111111111", JobRecord{ID: "job-one", CreatedAt: now, UpdatedAt: now}))

	appliance.WithStores(registry, jobs)
	require.NoError(t, appliance.Teardown(ctx, true))

	remainingIndexes, err := registry.ListAll(ctx)
	require.NoError(t, err)
	assert.Empty(t, remainingIndexes, "purgeMetadata=true must purge every index record")

	remainingJobs, err := jobs.ListAll(ctx)
	require.NoError(t, err)
	assert.Empty(t, remainingJobs, "purgeMetadata=true must still purge every job record")
}

// TestTeardown_DefaultPreservesKnowledgeBasesAndDataSources proves a default
// Teardown leaves gateway-owned KB/DataSource records intact, mirroring the
// registry: their SourceSpec is what a later reconcile re-ingests from.
func TestTeardown_DefaultPreservesKnowledgeBasesAndDataSources(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	masterKey := testMasterKey(t)
	launcher := &fakeLauncher{endpoint: "10.0.0.35", port: 5432}
	appliance, err := NewAppliance(js, masterKey, launcher)
	require.NoError(t, err)
	seedAvailableAppliance(t, appliance, masterKey, "10.0.0.35", 5432)

	kb := NewKBStore(js)
	ds := NewDataSourceStore(js)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, kb.Create(ctx, "111111111111", KBRecord{ID: "kb-one", IndexID: "idx-one", CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, ds.Create(ctx, "111111111111", DataSourceRecord{ID: "ds-one", KnowledgeBaseID: "kb-one", CreatedAt: now, UpdatedAt: now}))

	appliance.WithKBStores(kb, ds)
	require.NoError(t, appliance.Teardown(ctx, false))

	remainingKBs, err := kb.List(ctx, "111111111111")
	require.NoError(t, err)
	assert.Len(t, remainingKBs, 1, "default Teardown must leave knowledge base records intact")

	remainingDS, err := ds.List(ctx, "111111111111")
	require.NoError(t, err)
	assert.Len(t, remainingDS, 1, "default Teardown must leave data source records intact")
}

// TestTeardown_PurgeMetadataPurgesKnowledgeBasesAndDataSources proves
// purgeMetadata=true restores the old full wipe of the gateway-owned
// knowledge-base and data-source records.
func TestTeardown_PurgeMetadataPurgesKnowledgeBasesAndDataSources(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	masterKey := testMasterKey(t)
	launcher := &fakeLauncher{endpoint: "10.0.0.37", port: 5432}
	appliance, err := NewAppliance(js, masterKey, launcher)
	require.NoError(t, err)
	seedAvailableAppliance(t, appliance, masterKey, "10.0.0.37", 5432)

	kb := NewKBStore(js)
	ds := NewDataSourceStore(js)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, kb.Create(ctx, "111111111111", KBRecord{ID: "kb-one", IndexID: "idx-one", CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, ds.Create(ctx, "111111111111", DataSourceRecord{ID: "ds-one", KnowledgeBaseID: "kb-one", CreatedAt: now, UpdatedAt: now}))

	appliance.WithKBStores(kb, ds)
	require.NoError(t, appliance.Teardown(ctx, true))

	remainingKBs, err := kb.List(ctx, "111111111111")
	require.NoError(t, err)
	assert.Empty(t, remainingKBs, "purgeMetadata=true must purge every knowledge base record")

	remainingDS, err := ds.List(ctx, "111111111111")
	require.NoError(t, err)
	assert.Empty(t, remainingDS, "purgeMetadata=true must purge every data source record")
}

// TestTeardown_PurgeMetadataSurfacesStorePurgeErrors proves a purgeMetadata=
// true Teardown reports (rather than swallows) a failure from any of
// registry/kb/ds PurgeAll, joined alongside every other step's own errors.
func TestTeardown_PurgeMetadataSurfacesStorePurgeErrors(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	masterKey := testMasterKey(t)
	launcher := &fakeLauncher{endpoint: "10.0.0.39", port: 5432}
	appliance, err := NewAppliance(js, masterKey, launcher)
	require.NoError(t, err)
	seedAvailableAppliance(t, appliance, masterKey, "10.0.0.39", 5432)

	registry := NewRegistry(js)
	jobs := NewJobStore(js)
	kb := NewKBStore(js)
	ds := NewDataSourceStore(js)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, registry.Reserve(ctx, "111111111111", Record{ID: "idx-one", CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, kb.Create(ctx, "111111111111", KBRecord{ID: "kb-one", IndexID: "idx-one", CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, ds.Create(ctx, "111111111111", DataSourceRecord{ID: "ds-one", KnowledgeBaseID: "kb-one", CreatedAt: now, UpdatedAt: now}))

	appliance.WithStores(registry, jobs)
	appliance.WithKBStores(kb, ds)

	// Sever the shared NATS connection so every store's PurgeAll call fails,
	// without any production code change.
	nc.Close()

	err = appliance.Teardown(ctx, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "purge index registry")
	assert.Contains(t, err.Error(), "purge knowledge bases")
	assert.Contains(t, err.Error(), "purge data sources")
}

// TestTeardown_WithoutStoresStillWorks proves an Appliance that never calls
// WithStores (every pre-existing caller and test) still tears down cleanly:
// the registry/jobs purge is nil-safe, not a new required dependency.
func TestTeardown_WithoutStoresStillWorks(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	masterKey := testMasterKey(t)
	launcher := &fakeLauncher{endpoint: "10.0.0.34", port: 5432}
	appliance, err := NewAppliance(js, masterKey, launcher)
	require.NoError(t, err)
	seedAvailableAppliance(t, appliance, masterKey, "10.0.0.34", 5432)

	require.NoError(t, appliance.Teardown(context.Background(), false))
}

// TestBucket_RequiresJetStream proves an Appliance built with no JetStream
// client fails fast, and says which client is missing, rather than panicking
// on first use.
func TestBucket_RequiresJetStream(t *testing.T) {
	appliance, err := NewAppliance(nil, testMasterKey(t), &fakeLauncher{})
	require.NoError(t, err)

	_, err = appliance.store.KV(t.Context())
	require.Error(t, err)
	assert.ErrorContains(t, err, "appliance has no JetStream client configured")
}

// TestBuildDSN proves the DSN round-trips endpoint/port/username/password,
// percent-encoding special characters via net/url rather than concatenation.
func TestBuildDSN(t *testing.T) {
	dsn := buildDSN("10.0.0.1", 5432, "ochre_vector_admin", "p@ss/word?")
	parsed, err := url.Parse(dsn)
	require.NoError(t, err)
	assert.Equal(t, "postgres", parsed.Scheme)
	assert.Equal(t, "10.0.0.1:5432", parsed.Host)
	assert.Equal(t, "/"+AppliancePostgresDatabase, parsed.Path)
	assert.Equal(t, "ochre_vector_admin", parsed.User.Username())
	pw, ok := parsed.User.Password()
	assert.True(t, ok)
	assert.Equal(t, "p@ss/word?", pw)
}

// TestDecryptPassword_WrongKeyErrors proves decryptPassword fails rather
// than returning garbage when the appliance's masterKey does not match the
// key the record was encrypted with.
func TestDecryptPassword_WrongKeyErrors(t *testing.T) {
	rightKey := testMasterKey(t)
	wrongKey := testMasterKey(t)

	password, err := generateMasterPassword()
	require.NoError(t, err)
	encrypted, err := handlers_iam.EncryptSecret(password, rightKey)
	require.NoError(t, err)

	appliance := &Appliance{masterKey: wrongKey}
	_, err = appliance.decryptPassword(&ApplianceRecord{EncryptedPassword: encrypted})
	assert.Error(t, err)
}

// TestNewApplianceRecord_EncryptFailureOnBadKeyLength proves newApplianceRecord
// surfaces an encrypt failure rather than silently persisting a plaintext or
// unencrypted password.
func TestNewApplianceRecord_EncryptFailureOnBadKeyLength(t *testing.T) {
	_, _, err := newApplianceRecord([]byte("too-short"))
	assert.Error(t, err)
}

// TestGetRecord_MalformedJSON proves a corrupted record is a hard decode
// error, not a silently empty/zero record.
func TestGetRecord_MalformedJSON(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	appliance, err := NewAppliance(js, testMasterKey(t), &fakeLauncher{})
	require.NoError(t, err)

	ctx := context.Background()
	kv := applianceRawKV(t, appliance)
	_, err = kv.Put(ctx, appliancePostgresKey, []byte("not json"))
	require.NoError(t, err)

	_, _, err = appliance.getRecord(ctx)
	assert.Error(t, err)
}

// TestWaitOrResume_UnexpectedStateErrors proves an appliance record in a
// state neither PROVISIONING nor AVAILABLE is a hard error, not a silent
// wait forever.
func TestWaitOrResume_UnexpectedStateErrors(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	masterKey := testMasterKey(t)
	appliance, err := NewAppliance(js, masterKey, &fakeLauncher{})
	require.NoError(t, err)

	now := time.Now().UTC()
	seed := ApplianceRecord{
		Identifier:     ApplianceIdentifier,
		MasterUsername: applianceMasterUsername,
		State:          "BOGUS",
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	data, err := json.Marshal(seed)
	require.NoError(t, err)

	ctx := context.Background()
	kv := applianceRawKV(t, appliance)
	_, err = kv.Create(ctx, appliancePostgresKey, data)
	require.NoError(t, err)

	_, err = appliance.waitOrResume(ctx)
	assert.Error(t, err)
}

// TestWaitOrResume_VanishedRecordRetriesClaim proves a losing caller whose
// record disappears before it can be read (e.g. an operator delete) retries
// the claim from scratch rather than erroring.
func TestWaitOrResume_VanishedRecordRetriesClaim(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	launcher := &fakeLauncher{endpoint: "10.0.0.10", port: 5432}
	appliance, err := NewAppliance(js, testMasterKey(t), launcher)
	require.NoError(t, err)

	ctx := context.Background()

	info, err := appliance.waitOrResume(ctx)
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.10", info.Endpoint)
	assert.Equal(t, 1, launcher.callCount())
}

// TestPromote_ConflictFallsBackToCurrentAvailable proves promote treats a
// revision conflict against an already-AVAILABLE record (a concurrent
// resume won first) as success, not an error to surface.
func TestPromote_ConflictFallsBackToCurrentAvailable(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	appliance, err := NewAppliance(js, testMasterKey(t), &fakeLauncher{})
	require.NoError(t, err)

	rec, _, err := newApplianceRecord(appliance.masterKey)
	require.NoError(t, err)
	data, err := json.Marshal(rec)
	require.NoError(t, err)

	ctx := context.Background()
	kv := applianceRawKV(t, appliance)
	rev, err := kv.Create(ctx, appliancePostgresKey, data)
	require.NoError(t, err)

	already := rec
	already.State = ApplianceStateAvailable
	already.Endpoint = "10.0.0.20"
	already.Port = 5432
	alreadyData, err := json.Marshal(already)
	require.NoError(t, err)
	_, err = kv.Update(ctx, appliancePostgresKey, alreadyData, rev)
	require.NoError(t, err)

	info, err := appliance.promote(ctx, rec, rev, "10.0.0.21", 5432)
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.20", info.Endpoint)
}
