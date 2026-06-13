package handlers_eks

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// markDeleting transitions a seeded cluster to DELETING and backdates
// DeletingSince so the reaper treats it as wedged (past min-age).
func markDeleting(t *testing.T, f *deleteClusterFixture, name string, age time.Duration) {
	t.Helper()
	require.NoError(t, SetClusterStatus(f.kv, name, ClusterStatusDeleting))
	meta, err := GetClusterMeta(f.kv, name)
	require.NoError(t, err)
	meta.DeletingSince = time.Now().UTC().Add(-age)
	require.NoError(t, PutClusterMeta(f.kv, meta))
}

// TestRLC4_DeletingReaperReDrivesWedgedTeardown locks mulga-siv-295.11: a cluster
// stuck in DELETING past min-age (its synchronous DeleteCluster failed and no
// client re-issued) must be re-driven to completion by the backstop reaper —
// infra torn down and meta swept — so its billable EIP is never stranded.
func TestRLC4_DeletingReaperReDrivesWedgedTeardown(t *testing.T) {
	f := newDeleteClusterFixture(t, "alpha")
	markDeleting(t, f, "alpha", 10*time.Minute)

	reaper := f.svc.NewDeletingReaper()
	n, err := reaper.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n, "the wedged DELETING cluster must be re-driven")

	_, getErr := GetClusterMeta(f.kv, "alpha")
	assert.ErrorIs(t, getErr, ErrClusterNotFound, "meta must be swept after the backstop completes teardown")
	assert.GreaterOrEqual(t, len(f.inst.terminateCalls), 1, "CP VM must be terminated")
	assert.Len(t, f.eip.releaseCalls, 1, "the billable egress EIP must be released, not stranded")
}

// TestDeletingReaperSkipsFreshDelete: a cluster that just entered DELETING is
// within the in-flight synchronous-delete window; the reaper must not race it.
func TestDeletingReaperSkipsFreshDelete(t *testing.T) {
	f := newDeleteClusterFixture(t, "beta")
	markDeleting(t, f, "beta", 1*time.Second) // younger than min-age

	reaper := f.svc.NewDeletingReaper()
	n, err := reaper.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, n, "a freshly-DELETING cluster must be left to its in-flight delete")

	meta, getErr := GetClusterMeta(f.kv, "beta")
	require.NoError(t, getErr, "meta must survive — the reaper did not re-drive")
	assert.Equal(t, ClusterStatusDeleting, meta.Status)
	assert.Empty(t, f.inst.terminateCalls, "no teardown must run within the min-age window")
}

// TestDeletingReaperSkipsNonDeleting: a CREATING/ACTIVE cluster is never touched.
func TestDeletingReaperSkipsNonDeleting(t *testing.T) {
	f := newDeleteClusterFixture(t, "gamma") // stays CREATING

	reaper := f.svc.NewDeletingReaper()
	n, err := reaper.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	meta, getErr := GetClusterMeta(f.kv, "gamma")
	require.NoError(t, getErr)
	assert.Equal(t, ClusterStatusCreating, meta.Status)
	assert.Empty(t, f.inst.terminateCalls)
}
