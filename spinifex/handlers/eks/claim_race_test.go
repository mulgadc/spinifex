package handlers_eks

import (
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// classifyCreateErrs splits a slice of create results into success vs
// ResourceInUse counts, failing the test on any other error.
func classifyCreateErrs(t *testing.T, errs []error) (ok, inUse int) {
	t.Helper()
	for _, e := range errs {
		switch {
		case e == nil:
			ok++
		case e.Error() == awserrors.ErrorEKSResourceInUse:
			inUse++
		default:
			t.Fatalf("unexpected create error: %v", e)
		}
	}
	return ok, inUse
}

// Two concurrent CreateCluster calls for the same name must resolve to exactly
// one owner (CREATING) and one ResourceInUse reject. The atomic name claim is
// the first mutating step, so the loser returns before any VM/NLB launch —
// asserted structurally here and enforced under -race (a double-claim would
// race the launcher fakes the winner mutates).
func TestCreateCluster_ConcurrentSameNameSingleOwner(t *testing.T) {
	f := newEKSServiceFixture(t)

	const n = 2
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.svc.CreateCluster(createInput("race"), testAccountID, "")
		}(i)
	}
	wg.Wait()
	f.svc.WaitLaunches()

	ok, inUse := classifyCreateErrs(t, errs)
	assert.Equal(t, 1, ok, "exactly one create owns the name")
	assert.Equal(t, 1, inUse, "the duplicate is rejected ResourceInUse")

	meta, err := GetClusterMeta(f.kv, "race")
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusCreating, meta.Status)
}

// A FAILED cluster plus two concurrent retries must reclaim exactly once: one
// caller CAS-flips FAILED→CREATING and proceeds, the other loses the CAS and is
// rejected ResourceInUse. No double purge/relaunch.
func TestCreateCluster_ConcurrentReclaimSingleOwner(t *testing.T) {
	f := newEKSServiceFixture(t)

	meta := sampleClusterMeta("race")
	meta.Status = ClusterStatusFailed
	require.NoError(t, PutClusterMeta(f.kv, meta))

	const n = 2
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.svc.CreateCluster(createInput("race"), testAccountID, "")
		}(i)
	}
	wg.Wait()
	f.svc.WaitLaunches()

	ok, inUse := classifyCreateErrs(t, errs)
	assert.Equal(t, 1, ok, "exactly one retry reclaims the FAILED cluster")
	assert.Equal(t, 1, inUse, "the concurrent retry is rejected")

	got, err := GetClusterMeta(f.kv, "race")
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusCreating, got.Status)
}

// Two concurrent CreateNodegroup calls for the same name must resolve to one
// owner and one ResourceInUse reject, and the worker launcher must record
// exactly one launch (the loser returns at the record claim before
// launchWorkers).
func TestCreateNodegroup_ConcurrentSameNameSingleOwner(t *testing.T) {
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")

	const n = 2
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.svc.CreateNodegroup(createNGInput("c1", "ng1", 1), testAccountID)
		}(i)
	}
	wg.Wait()
	// The single winning owner launches one worker; let its Ready-gate resolve.
	markWorkersReady(t, f, "c1", 1)
	f.svc.WaitLaunches()

	ok, inUse := classifyCreateErrs(t, errs)
	assert.Equal(t, 1, ok, "exactly one create owns the nodegroup")
	assert.Equal(t, 1, inUse, "the duplicate is rejected ResourceInUse")
	assert.Len(t, f.worker.runCalls, 1, "only the owner launches a worker; no double-launch")
}
