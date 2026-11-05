package accountlifecycle

//test:in-package — the convergence loop drives unexported engine fields and
// the unexported backoff hook.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/accountcleanup"
)

// scriptedStage answers each pass from a script, repeating the last answer.
type scriptedStage struct {
	cleanups   []*accountcleanup.Outcome
	remainders []*accountcleanup.Outcome
	cleanupN   int
	remainN    int
	forces     []bool
}

func pick(list []*accountcleanup.Outcome, n int) *accountcleanup.Outcome {
	if n >= len(list) {
		n = len(list) - 1
	}
	return list[n]
}

func (s *scriptedStage) run(forceIn bool) *accountcleanup.Outcome {
	s.forces = append(s.forces, forceIn)
	out := pick(s.cleanups, s.cleanupN)
	s.cleanupN++
	return out
}

func (s *scriptedStage) remaining() *accountcleanup.Outcome {
	out := pick(s.remainders, s.remainN)
	s.remainN++
	return out
}

type scriptedCompute struct{ scriptedStage }

func (s *scriptedCompute) CleanupCompute(_ context.Context, req *accountcleanup.ComputeRequest) (*accountcleanup.ComputeResult, error) {
	return s.run(req.Force), nil
}

func (s *scriptedCompute) ComputeRemaining(context.Context, string) (*accountcleanup.Outcome, error) {
	return s.remaining(), nil
}

type scriptedStorage struct{ scriptedStage }

func (s *scriptedStorage) CleanupStorage(_ context.Context, req *accountcleanup.StorageRequest) (*accountcleanup.StorageResult, error) {
	return s.run(req.Force), nil
}

func (s *scriptedStorage) StorageRemaining(context.Context, string) (*accountcleanup.Outcome, error) {
	return s.remaining(), nil
}

func outcome(items ...accountcleanup.Item) *accountcleanup.Outcome {
	out := &accountcleanup.Outcome{AccountID: testAccountID, Status: accountcleanup.StatusClean, Items: items}
	return out
}

func incomplete(reason string) *accountcleanup.Outcome {
	return &accountcleanup.Outcome{AccountID: testAccountID, Status: accountcleanup.StatusIncomplete, IncompleteReasons: []string{reason}}
}

func item(kind, id, state, disposition, reason string) accountcleanup.Item {
	return accountcleanup.Item{Kind: kind, ResourceID: id, State: state, Disposition: disposition, Reason: reason}
}

// convergeEngine runs a teardown with no real delay between passes.
func convergeEngine(accounts AccountStore, compute *scriptedCompute, storage *scriptedStorage) (*Engine, *int) {
	engine := quietEngine(accounts)
	engine.Compute, engine.Storage = compute, storage
	waits := 0
	engine.wait = func(ctx context.Context, _ time.Duration) error {
		waits++
		return ctx.Err()
	}
	return engine, &waits
}

var (
	runningInstance     = item(accountcleanup.KindInstance, "i-1", "running", accountcleanup.DispositionPending, accountcleanup.ReasonTerminating)
	termInstance        = item(accountcleanup.KindInstance, "i-1", "terminated", accountcleanup.DispositionDeleted, "")
	blockedHolderVolume = item(accountcleanup.KindVolume, "vol-1", "in-use", accountcleanup.DispositionBlocked, accountcleanup.ReasonHolderPresent)
)

func TestConvergeRunningInstanceFinishesInOneJob(t *testing.T) {
	compute := &scriptedCompute{scriptedStage{
		cleanups:   []*accountcleanup.Outcome{outcome(runningInstance), outcome(termInstance)},
		remainders: []*accountcleanup.Outcome{outcome(item(accountcleanup.KindInstance, "i-1", "shutting-down", accountcleanup.DispositionResidue, accountcleanup.ReasonRecordRemains)), outcome()},
	}}
	storage := &scriptedStorage{scriptedStage{
		cleanups:   []*accountcleanup.Outcome{outcome(blockedHolderVolume), outcome(item(accountcleanup.KindVolume, "vol-1", "available", accountcleanup.DispositionDeleted, ""))},
		remainders: []*accountcleanup.Outcome{outcome(item(accountcleanup.KindVolume, "vol-1", "in-use", accountcleanup.DispositionResidue, accountcleanup.ReasonRecordRemains)), outcome()},
	}}
	accounts := newFakeAccounts()
	engine, waits := convergeEngine(accounts, compute, storage)

	result, err := engine.Teardown(context.Background(), request())
	if err != nil {
		t.Fatalf("a running instance must converge in one job, got %v", err)
	}
	if !result.AccountDeleted || !accounts.deleted {
		t.Fatalf("account must be deleted once the loop ends clean: %+v", result)
	}
	if *waits != 1 || compute.cleanupN != 2 || storage.cleanupN != 2 {
		t.Fatalf("expected two passes and one backoff, got passes=%d/%d waits=%d", compute.cleanupN, storage.cleanupN, *waits)
	}
}

func TestConvergeStopsOnBlockerTimeCannotResolve(t *testing.T) {
	managed := item(accountcleanup.KindInstance, "i-managed", "running", accountcleanup.DispositionBlocked, accountcleanup.ReasonManagedServiceOwner)
	remain := item(accountcleanup.KindInstance, "i-managed", "running", accountcleanup.DispositionResidue, accountcleanup.ReasonRecordRemains)
	compute := &scriptedCompute{scriptedStage{cleanups: []*accountcleanup.Outcome{outcome(managed)}, remainders: []*accountcleanup.Outcome{outcome(remain)}}}
	storage := &scriptedStorage{scriptedStage{cleanups: []*accountcleanup.Outcome{outcome()}, remainders: []*accountcleanup.Outcome{outcome()}}}
	accounts := newFakeAccounts()
	engine, _ := convergeEngine(accounts, compute, storage)
	engine.Converge = Converge{Budget: time.Hour}

	result, err := engine.Teardown(context.Background(), request())
	if !errors.Is(err, ErrResourcesStuck) {
		t.Fatalf("a non-converging blocker must end the job stuck, got %v", err)
	}
	if accounts.deleted {
		t.Fatal("account must not be deleted while an instance remains")
	}
	if compute.cleanupN > 2 {
		t.Fatalf("loop must end before the budget on a blocker time cannot resolve, ran %d passes", compute.cleanupN)
	}
	var named bool
	for _, stage := range result.Stages {
		for _, s := range stage.Stuck {
			named = named || (s.Resource.ID == "i-managed" && s.Reason == accountcleanup.ReasonManagedServiceOwner)
		}
	}
	if !named {
		t.Fatalf("stuck item must be named with the cleanup's reason: %+v", result.Stages)
	}
}

// supersededAccounts reports a different holder once the first cleanup ran.
type supersededAccounts struct {
	*fakeAccounts

	after *scriptedCompute
}

func (s *supersededAccounts) GetAccount(id string) (*Account, error) {
	account, err := s.fakeAccounts.GetAccount(id)
	if err != nil || s.after.cleanupN == 0 {
		return account, err
	}
	taken := *account
	taken.TerminationOp = &TerminationOp{OperationID: "other-op", Fence: account.TerminationOp.Fence + 1}
	return &taken, nil
}

func TestConvergeLostFenceStopsBeforeNextPass(t *testing.T) {
	compute := &scriptedCompute{scriptedStage{
		cleanups:   []*accountcleanup.Outcome{outcome(runningInstance)},
		remainders: []*accountcleanup.Outcome{outcome(item(accountcleanup.KindInstance, "i-1", "shutting-down", accountcleanup.DispositionResidue, accountcleanup.ReasonRecordRemains))},
	}}
	storage := &scriptedStorage{scriptedStage{cleanups: []*accountcleanup.Outcome{outcome()}, remainders: []*accountcleanup.Outcome{outcome()}}}
	accounts := &supersededAccounts{fakeAccounts: newFakeAccounts(), after: compute}
	engine, _ := convergeEngine(accounts, compute, storage)

	// The deadline lets a loop that ignores the lost fence end, so it fails
	// the pass count below instead of hanging.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := engine.Teardown(ctx, request())
	if err == nil || errors.Is(err, ErrResourcesStuck) {
		t.Fatalf("a lost fence must stop the loop with an error, got %v", err)
	}
	if compute.cleanupN != 1 {
		t.Fatalf("no terminate may be issued after the fence is lost, ran %d passes", compute.cleanupN)
	}
	if accounts.deleted {
		t.Fatal("a superseded holder must not delete the account")
	}
}

func TestConvergeCancelledContextStopsLoop(t *testing.T) {
	compute := &scriptedCompute{scriptedStage{
		cleanups:   []*accountcleanup.Outcome{outcome(runningInstance)},
		remainders: []*accountcleanup.Outcome{outcome(item(accountcleanup.KindInstance, "i-1", "shutting-down", accountcleanup.DispositionResidue, accountcleanup.ReasonRecordRemains))},
	}}
	storage := &scriptedStorage{scriptedStage{cleanups: []*accountcleanup.Outcome{outcome()}, remainders: []*accountcleanup.Outcome{outcome()}}}
	engine, _ := convergeEngine(newFakeAccounts(), compute, storage)
	ctx, cancel := context.WithCancel(context.Background())
	engine.OnPass = func(context.Context, int) { cancel() }

	if _, err := engine.Teardown(ctx, request()); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled job must stop the loop, got %v", err)
	}
	if compute.cleanupN != 1 {
		t.Fatalf("no pass may start after cancel, ran %d", compute.cleanupN)
	}
}

func TestConvergeIncompleteReadsEndStuckNotClean(t *testing.T) {
	compute := &scriptedCompute{scriptedStage{cleanups: []*accountcleanup.Outcome{incomplete(accountcleanup.ReasonInstanceInventoryIncomplete)}, remainders: []*accountcleanup.Outcome{incomplete(accountcleanup.ReasonInstanceInventoryIncomplete)}}}
	storage := &scriptedStorage{scriptedStage{cleanups: []*accountcleanup.Outcome{outcome()}, remainders: []*accountcleanup.Outcome{outcome()}}}
	accounts := newFakeAccounts()
	engine, _ := convergeEngine(accounts, compute, storage)

	_, err := engine.Teardown(context.Background(), request())
	if !errors.Is(err, ErrResourcesStuck) || accounts.deleted {
		t.Fatalf("an unreadable inventory must never read as an empty account: err=%v deleted=%v", err, accounts.deleted)
	}
}

// The loop keeps the account while the storage owner reports a volume still
// recorded attached. That the owner refuses such a volume is the owner's own
// invariant test in accountcleanup, against its real cleanup.
func TestConvergeKeepsTheAccountWhileAnAttachedVolumeRemains(t *testing.T) {
	compute := &scriptedCompute{scriptedStage{cleanups: []*accountcleanup.Outcome{outcome()}, remainders: []*accountcleanup.Outcome{outcome()}}}
	storage := &scriptedStorage{scriptedStage{
		cleanups:   []*accountcleanup.Outcome{outcome(blockedHolderVolume)},
		remainders: []*accountcleanup.Outcome{outcome(item(accountcleanup.KindVolume, "vol-1", "in-use", accountcleanup.DispositionResidue, accountcleanup.ReasonRecordRemains))},
	}}
	for _, force := range []bool{false, true} {
		accounts := newFakeAccounts()
		engine, _ := convergeEngine(accounts, compute, storage)
		req := request()
		req.Force = force
		storage.cleanupN, storage.remainN = 0, 0

		_, err := engine.Teardown(context.Background(), req)
		if !errors.Is(err, ErrResourcesStuck) || accounts.deleted {
			t.Fatalf("force=%v: a volume still recorded attached must keep the account, err=%v deleted=%v", force, err, accounts.deleted)
		}
	}
}

func TestConvergeMissingCleanerFailsClosed(t *testing.T) {
	engine := quietEngine(newFakeAccounts())
	engine.Storage = nil
	if _, err := engine.Teardown(context.Background(), request()); !errors.Is(err, errComputeCleanupMissing) {
		t.Fatalf("compute without storage must refuse to start, got %v", err)
	}
}

// cleanStages are compute and storage cleanups of an account that holds nothing.
func cleanStages() (*scriptedCompute, *scriptedStorage) {
	return &scriptedCompute{scriptedStage{cleanups: []*accountcleanup.Outcome{outcome()}, remainders: []*accountcleanup.Outcome{outcome()}}},
		&scriptedStorage{scriptedStage{cleanups: []*accountcleanup.Outcome{outcome()}, remainders: []*accountcleanup.Outcome{outcome()}}}
}
