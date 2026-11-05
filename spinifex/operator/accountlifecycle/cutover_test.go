package accountlifecycle

//test:in-package — these drive the engine's unexported stage wiring with
// scripted compute and storage owners, so each can count what it was asked.

import (
	"context"
	"errors"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/accountcleanup"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failingRemainder answers the compute remainder read until its nth call.
type failingRemainder struct {
	ComputeStage

	failOn int
	calls  int
}

func (f *failingRemainder) ComputeRemaining(ctx context.Context, accountID string) (*accountcleanup.Outcome, error) {
	f.calls++
	if f.calls == f.failOn {
		return nil, errors.New("owner unreachable")
	}
	return f.ComputeStage.ComputeRemaining(ctx, accountID)
}

func stuckIDs(result *Result) []string {
	var ids []string
	for _, stage := range result.Stages {
		for _, s := range stage.Stuck {
			ids = append(ids, s.Resource.ID)
		}
	}
	return ids
}

// An account deletion that was never accepted must reach no terminate and no
// volume delete: the quiesce refuses first, whatever Force says.
func TestS7_NoTerminateOrVolumeDeleteWithoutAnAcceptedDeletion(t *testing.T) {
	for _, force := range []bool{false, true} {
		accounts := newFakeAccounts()
		accounts.statusErr = errors.New("account is owned by a newer fence")
		// The record already reads as ours, so only the quiesce itself stands
		// between this holder and the terminate.
		accounts.account.Status = AccountStatusTerminating
		accounts.account.TerminationOp = &TerminationOp{OperationID: "op-1", Fence: 1}
		engine := quietEngine(accounts)
		compute, storage := cleanStages()
		engine.Compute, engine.Storage = compute, storage

		req := request()
		req.OperationID, req.Fence, req.Force = "op-1", 1, force
		_, err := engine.Teardown(context.Background(), req)

		require.Error(t, err)
		assert.Zero(t, compute.cleanupN, "force=%v: no terminate", force)
		assert.Zero(t, storage.cleanupN, "force=%v: no volume delete", force)
		assert.False(t, accounts.deleted)
	}
}

// With the compute and storage owners missing the teardown refuses before it
// quiesces anything, so a mis-wired gateway cannot half-delete an account.
func TestS7_MissingComputeOwnerRefusesBeforeAnyStageRuns(t *testing.T) {
	accounts := newFakeAccounts()
	engine := quietEngine(accounts)
	engine.Compute = nil

	_, err := engine.Teardown(context.Background(), request())

	require.ErrorIs(t, err, errComputeCleanupMissing)
	assert.Empty(t, accounts.quiesces)
	assert.False(t, accounts.deleted)
}

// A resource that appears after the loop ended, while the later stages ran,
// still keeps the account, and it is named.
func TestFinalRereadKeepsTheAccountWhenAnInstanceArrivesLate(t *testing.T) {
	accounts := newFakeAccounts()
	engine := quietEngine(accounts)
	engine.Compute = &scriptedCompute{scriptedStage{
		cleanups:   []*accountcleanup.Outcome{outcome()},
		remainders: []*accountcleanup.Outcome{outcome(), outcome(item(accountcleanup.KindInstance, "i-late", "running", accountcleanup.DispositionResidue, accountcleanup.ReasonRecordRemains))},
	}}

	result, err := engine.Teardown(context.Background(), request())

	require.ErrorIs(t, err, ErrResourcesStuck)
	assert.False(t, accounts.deleted)
	assert.Contains(t, stuckIDs(result), "i-late")
}

// A final read that cannot be answered proves nothing, so the account stays.
func TestFinalRereadKeepsTheAccountWhenTheReadFails(t *testing.T) {
	accounts := newFakeAccounts()
	engine := quietEngine(accounts)
	compute, _ := cleanStages()
	engine.Compute = &failingRemainder{ComputeStage: compute, failOn: 2}

	result, err := engine.Teardown(context.Background(), request())

	require.ErrorIs(t, err, ErrResourcesStuck)
	assert.False(t, accounts.deleted)
	assert.Contains(t, stuckIDs(result), StageAttachments.String())
}

// An ECS, EKS or RDS owner that cannot finish deleting may be running on these
// instances, so none is terminated under it.
func TestStuckComputeOwnerLeavesTheInstancesAlone(t *testing.T) {
	accounts := newFakeAccounts()
	cluster := &fakeReaper{
		kind: "ecs-cluster", stage: StageCompute, blockedUntilForce: true,
		resources: []Resource{{Kind: "ecs-cluster", ID: "web"}},
	}
	engine := quietEngine(accounts, cluster)
	compute, _ := cleanStages()
	engine.Compute = compute

	result, err := engine.Teardown(context.Background(), request())

	require.ErrorIs(t, err, ErrResourcesStuck)
	assert.Zero(t, compute.cleanupN, "no instance is terminated while the owner holds resources")
	assert.False(t, accounts.deleted)
	assert.Contains(t, stuckIDs(result), "web")
}

// The dry run reports what the actual deletion would destroy.
func TestDryRunListsInstancesSpotRequestsAndVolumes(t *testing.T) {
	accounts := newFakeAccounts()
	engine := quietEngine(accounts)
	engine.Compute = &scriptedCompute{scriptedStage{
		cleanups: []*accountcleanup.Outcome{outcome()},
		remainders: []*accountcleanup.Outcome{outcome(
			item(accountcleanup.KindInstance, "i-1", "running", accountcleanup.DispositionPending, ""),
			item(accountcleanup.KindSpotRequest, "sir-1", "open", accountcleanup.DispositionPending, ""),
		)},
	}}
	engine.Storage = &scriptedStorage{scriptedStage{
		cleanups:   []*accountcleanup.Outcome{outcome()},
		remainders: []*accountcleanup.Outcome{outcome(item(accountcleanup.KindVolume, "vol-1", "available", accountcleanup.DispositionPending, ""))},
	}}

	result, err := engine.DryRun(context.Background(), testAccountID)

	require.NoError(t, err)
	var ids []string
	for _, stage := range result.Stages {
		for _, r := range stage.Deleted {
			ids = append(ids, r.ID)
		}
	}
	assert.ElementsMatch(t, []string{"i-1", "sir-1", "vol-1"}, ids)
}

// A dry run that cannot read the owners fails rather than listing less.
func TestDryRunFailsWhenTheComputeOwnerCannotBeRead(t *testing.T) {
	engine := quietEngine(newFakeAccounts())
	compute, _ := cleanStages()
	engine.Compute = &failingRemainder{ComputeStage: compute, failOn: 1}

	_, err := engine.DryRun(context.Background(), testAccountID)

	require.Error(t, err)
}
