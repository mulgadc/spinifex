package handlers_eks

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSpawnRegisteredReconcilers_ResumesNonTerminal models a daemon restart:
// every cluster still in CREATING or ACTIVE must get a reconciler re-registered
// so lifecycle reconcile resumes without waiting for the next CreateCluster,
// while terminal (FAILED/DELETING) clusters stay untouched.
func TestSpawnRegisteredReconcilers_ResumesNonTerminal(t *testing.T) {
	f := newEKSServiceFixture(t)

	active := sampleClusterMeta("alpha")
	active.Status = ClusterStatusActive
	require.NoError(t, PutClusterMeta(f.kv, active))

	// CREATING resumes both the bootstrap re-subscribe and the reconciler. Stamp a
	// recent CreatedAt so the resumed reconciler stays CREATING (the shared fixture
	// meta is weeks old, which trips the CREATE timeout and self-deregisters before
	// the assertion — the race this test previously flaked on).
	creating := sampleClusterMeta("beta")
	creating.Status = ClusterStatusCreating
	creating.CreatedAt = time.Now()
	require.NoError(t, PutClusterMeta(f.kv, creating))

	failed := sampleClusterMeta("zeta")
	failed.Status = ClusterStatusFailed
	require.NoError(t, PutClusterMeta(f.kv, failed))

	require.NoError(t, f.svc.SpawnRegisteredReconcilers())

	assert.True(t, f.svc.registry.Has(testAccountID, "alpha"), "ACTIVE cluster must resume a reconciler")
	assert.True(t, f.svc.registry.Has(testAccountID, "beta"), "CREATING cluster must resume a reconciler")
	assert.False(t, f.svc.registry.Has(testAccountID, "zeta"), "FAILED cluster must not be resumed")
}

// TestSpawnRegisteredReconcilers_DepsNotReadyNoops confirms the boot scan is a
// safe no-op when orchestration deps are absent (the shim construction path),
// so it never panics or registers phantom holders on a half-wired daemon.
func TestSpawnRegisteredReconcilers_DepsNotReadyNoops(t *testing.T) {
	svc := setupTestService(t)
	require.NoError(t, svc.SpawnRegisteredReconcilers())
	assert.False(t, svc.registry.Has(testAccountID, "alpha"))
}
