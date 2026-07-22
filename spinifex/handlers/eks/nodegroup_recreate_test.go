package handlers_eks

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Re-creating a nodegroup of the same name must not baseline its Ready-gate on
// the previous incarnation's workers.
//
// A terraform replace after CREATE_FAILED destroys and re-creates the same
// nodegroup name. The old workers are still Ready when the create begins, so a
// gate that baselines on the live count demands old+new Ready nodes — then the
// old ones are terminated, the count falls, and the target becomes unreachable.
// The observed effect was a cluster whose three new workers all joined being
// failed with "reports 3 Ready nodes, want >= 5 (baseline 2 + 3 workers)", and
// each retry raising the bar again.
func TestCreateNodegroup_RecreateDoesNotBaselineOnPriorWorkers(t *testing.T) {
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")

	// First incarnation: two workers register Ready and the nodegroup activates.
	_, err := f.svc.CreateNodegroup(context.Background(), createNGInput("c1", "ng", 2), testAccountID)
	require.NoError(t, err)
	markWorkersReady(t, f, "c1", "ng", 2)
	f.svc.WaitLaunches()

	rec, err := GetNodegroupRecord(f.kv, "c1", "ng")
	require.NoError(t, err)
	require.Equal(t, eks.NodegroupStatusActive, rec.Status)

	// A terraform replace deletes the nodegroup, then re-creates the same name.
	// The CP's Ready report lags the delete, so the previous incarnation's two
	// workers are still counted Ready when the create begins — exactly the state
	// that produced "want >= 4 (baseline 2 + 2 workers)" in the field.
	require.NoError(t, DeleteNodegroupRecord(f.kv, "c1", "ng"))
	markWorkersReady(t, f, "c1", "ng", 2)

	_, err = f.svc.CreateNodegroup(context.Background(), createNGInput("c1", "ng", 2), testAccountID)
	require.NoError(t, err)

	// The new incarnation's own two workers register Ready. With a zero
	// baseline this satisfies the gate; baselining on the prior count would
	// demand four and never resolve.
	markWorkersReady(t, f, "c1", "ng", 2)
	f.svc.WaitLaunches()

	rec, err = GetNodegroupRecord(f.kv, "c1", "ng")
	require.NoError(t, err)
	assert.Equal(t, eks.NodegroupStatusActive, rec.Status,
		"a re-created nodegroup whose own workers are Ready must reach ACTIVE, not CREATE_FAILED")
	assert.NotContains(t, rec.StatusReason, "did not become Ready")
}
