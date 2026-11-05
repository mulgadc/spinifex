package accountlifecycle

//test:in-package — the evaluator, its scope table and the engine hook are
// unexported, and the table is what keeps a scope from reading as quiesced by omission.

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
	operatorv1 "github.com/mulgadc/spinifex/contracts/operator/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func confirmedEvidence() QuiesceEvidence {
	return QuiesceEvidence{
		OperationID: "op-1", Fence: 2, Generation: 5,
		Account: &Account{
			AccountID: testAccountID, Status: AccountStatusTerminating, Generation: 5,
			TerminationOp: &TerminationOp{OperationID: "op-1", Fence: 2},
		},
		Inventory:   completeInventory(),
		EvaluatedAt: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC),
	}
}

func completeInventory() *ec2v1.PlatformInventoryReply {
	reply := &ec2v1.PlatformInventoryReply{AccountID: testAccountID}
	for _, kind := range ec2v1.PlatformKinds() {
		reply.Kinds = append(reply.Kinds, ec2v1.PlatformKindReport{Kind: kind, Status: ec2v1.PlatformStatusObservedClean})
	}
	return reply
}

func unresolvedFor(q operatorv1.IAMQuiescence, scope string) []operatorv1.UnresolvedScope {
	var out []operatorv1.UnresolvedScope
	for _, entry := range q.Unresolved {
		if entry.Scope == scope {
			out = append(out, entry)
		}
	}
	return out
}

// With the fence confirmed and every L09 kind scanned complete, the evidence is
// recorded but no kind is quiesced: nothing re-reads the inventory before the
// account record goes, so a late write would be orphaned (ADR-0008:S9 condition 3).
func TestEvaluateQuiescenceRecordsEvidenceButQuiescesNothingWithoutAFinalReread(t *testing.T) {
	q := EvaluateQuiescence(confirmedEvidence())

	assert.Equal(t, "op-1", q.OperationID)
	assert.Equal(t, uint64(2), q.Fence)
	assert.Equal(t, uint64(5), q.Generation)
	assert.Empty(t, q.QuiescedScope)
	assert.NotNil(t, q.QuiescedScope, "evaluated-and-empty must serialise as [] not null")
	for _, kind := range ec2PlatformKinds {
		assert.Equal(t, []operatorv1.UnresolvedScope{{
			Scope: "ec2-" + kind, Class: operatorv1.UnresolvedClassWriter,
			Reason: operatorv1.ReasonFinalInventoryRereadNotWired,
		}}, unresolvedFor(q, "ec2-"+kind))
	}
	assert.ElementsMatch(t, []operatorv1.InventoryStatus{
		{Kind: ec2v1.PlatformKindKeyPair, Status: ec2v1.PlatformStatusObservedClean},
		{Kind: ec2v1.PlatformKindPlacementGroup, Status: ec2v1.PlatformStatusObservedClean},
		{Kind: ec2v1.PlatformKindLaunchTemplate, Status: ec2v1.PlatformStatusObservedClean},
	}, q.Inventory)
	assert.Equal(t, []operatorv1.AccountedWriter{{Writer: "daemon-nats-handlers", Basis: "owner-inventory", Ref: "mulga-tpoz4"}}, q.Writers)
	assert.Equal(t, "2026-11-02T00:00:00Z", q.EvaluatedAt)
}

// Only an exercise, which re-reads the inventory after cleanup, names the complete kinds.
func TestEvaluateQuiescenceNamesCompleteKindsOnceTheRereadIsWired(t *testing.T) {
	ev := confirmedEvidence()
	ev.FinalRereadGuaranteed = true
	ev.Inventory.Kinds[1].Status = ec2v1.PlatformStatusScanIncomplete
	q := EvaluateQuiescence(ev)

	assert.Equal(t, []string{ec2v1.PlatformKindKeyPair, ec2v1.PlatformKindLaunchTemplate}, q.QuiescedScope)
	assert.Equal(t, operatorv1.ReasonOwnerInventoryIncomplete, unresolvedFor(q, "ec2-placement-group")[0].Reason)
}

func TestEvaluateQuiescenceRecordsAnIncompleteInventoryPerKind(t *testing.T) {
	cases := map[string]func(*QuiesceEvidence){
		"one kind scan-incomplete": func(ev *QuiesceEvidence) { ev.Inventory.Kinds[0].Status = ec2v1.PlatformStatusScanIncomplete },
		"kind missing":             func(ev *QuiesceEvidence) { ev.Inventory.Kinds = ev.Inventory.Kinds[1:] },
		"kind repeated": func(ev *QuiesceEvidence) {
			ev.Inventory.Kinds = append(ev.Inventory.Kinds, ev.Inventory.Kinds[0])
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			ev := confirmedEvidence()
			mutate(&ev)
			q := EvaluateQuiescence(ev)

			got := unresolvedFor(q, "ec2-key-pair")
			require.Len(t, got, 1)
			assert.Equal(t, operatorv1.ReasonOwnerInventoryIncomplete, got[0].Reason)
			assert.Equal(t, operatorv1.ReasonFinalInventoryRereadNotWired, unresolvedFor(q, "ec2-launch-template")[0].Reason,
				"the other kinds keep their own result")
		})
	}

	t.Run("no reply at all", func(t *testing.T) {
		ev := confirmedEvidence()
		ev.Inventory = nil
		q := EvaluateQuiescence(ev)
		for _, kind := range ec2PlatformKinds {
			assert.Equal(t, operatorv1.ReasonOwnerInventoryIncomplete, unresolvedFor(q, "ec2-"+kind)[0].Reason)
		}
	})
}

// Condition 1: a read-back that does not confirm the operation, fence and
// generation records nothing as quiesced and evaluates nothing else.
func TestEvaluateQuiescenceRecordsNothingWhenTheFenceIsNotConfirmed(t *testing.T) {
	cases := map[string]struct {
		mutate func(*QuiesceEvidence)
		reason string
	}{
		"not terminating":     {func(ev *QuiesceEvidence) { ev.Account.Status = "ACTIVE" }, operatorv1.ReasonFenceMismatch},
		"no operation":        {func(ev *QuiesceEvidence) { ev.Account.TerminationOp = nil }, operatorv1.ReasonFenceMismatch},
		"other operation":     {func(ev *QuiesceEvidence) { ev.Account.TerminationOp.OperationID = "op-2" }, operatorv1.ReasonFenceMismatch},
		"higher fence in IAM": {func(ev *QuiesceEvidence) { ev.Account.TerminationOp.Fence = 3 }, operatorv1.ReasonFenceMismatch},
		"other generation":    {func(ev *QuiesceEvidence) { ev.Account.Generation = 6 }, operatorv1.ReasonFenceMismatch},
		"read-back failed":    {func(ev *QuiesceEvidence) { ev.Account, ev.ReadbackErr = nil, errors.New("kv down") }, operatorv1.ReasonIAMReadbackFailed},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ev := confirmedEvidence()
			ev.FinalRereadGuaranteed = true
			tc.mutate(&ev)
			q := EvaluateQuiescence(ev)

			assert.Empty(t, q.QuiescedScope)
			assert.Equal(t, []operatorv1.UnresolvedScope{{Scope: "all", Class: operatorv1.UnresolvedClassFence, Reason: tc.reason}}, q.Unresolved)
			assert.Empty(t, q.Consumers)
			assert.Empty(t, q.Writers)
			assert.Empty(t, q.Inventory)
		})
	}
}

// Every scope outside the first exercise is named. The expected list is the
// lifecycle register's rows other than L01, L02 and L09, so a new row has to be
// added here and in the table rather than reading as quiesced by omission.
func TestEveryOtherScopeIsRecordedUnresolvedByName(t *testing.T) {
	want := []string{
		"ec2-instances", "ec2-spot-requests", "ebs-volumes", "ec2-snapshots-images", "ec2-vpc",
		"host-network", "ec2-account-settings", "ec2-tag-index", "iam", "elbv2", "acm", "eks", "ecs",
		"rds", "dns", "ochre", "quota", "idempotency", "s3", "ecr", "lambda", "sqs",
	}
	q := EvaluateQuiescence(confirmedEvidence())

	for _, scope := range want {
		got := unresolvedFor(q, scope)
		require.Len(t, got, 1, scope)
		assert.NotEmpty(t, got[0].Reason, scope)
	}
	assert.Len(t, q.Unresolved, len(want)+len(ec2PlatformKinds), "no scope beyond the register and the three kinds")
	for _, entry := range otherScopes {
		assert.False(t, slices.Contains(ec2PlatformKinds, entry.scope), entry.scope)
	}
}

// Predastore has no accepted enforcement contract, so the scopes it admits writes
// for stay unresolved and it is never cited as a consumer, even once the
// run is an exercise and every L09 kind is complete.
func TestPredastoreIsNeverCitedAndItsScopesNeverQuiesce(t *testing.T) {
	ev := confirmedEvidence()
	ev.FinalRereadGuaranteed = true
	q := EvaluateQuiescence(ev)

	for _, scope := range []string{"s3", "ecr"} {
		got := unresolvedFor(q, scope)
		require.Len(t, got, 1)
		assert.Equal(t, operatorv1.UnresolvedClassConsumer, got[0].Class)
		assert.Equal(t, operatorv1.ReasonNoAcceptedEnforcement, got[0].Reason)
		assert.NotContains(t, q.QuiescedScope, scope)
	}
	for _, consumer := range q.Consumers {
		assert.NotContains(t, consumer.Consumer, "predastore")
		want := "ADR-0005:S7"
		if consumer.Consumer == "sts-mint-gate" {
			want = "ADR-0008:S9 (IAM-owned)"
		}
		assert.Equal(t, want, consumer.Basis)
	}
	assert.Equal(t, []string{"awsgw-admission", "ecr-registry", "sts-mint-gate"},
		[]string{q.Consumers[0].Consumer, q.Consumers[1].Consumer, q.Consumers[2].Consumer})
}

type stubInventory struct {
	reply ec2v1.PlatformInventoryReply
	calls int
}

func (s *stubInventory) PlatformInventory(context.Context, string, string) ec2v1.PlatformInventoryReply {
	s.calls++
	return s.reply
}

// The hook fires once, after the quiesce and before the first stage, carrying the
// operation, fence and generation the quiesce returned.
func TestTeardownReportsQuiescenceAfterTheQuiesceAndBeforeTheFirstStage(t *testing.T) {
	accounts := newFakeAccounts()
	var events []string
	reaper := &fakeReaper{kind: "volume", stage: StageStorage, resources: []Resource{{Kind: "volume", ID: "vol-1"}}}
	engine := quietEngine(accounts, reaper)
	inventory := &stubInventory{reply: *completeInventory()}
	engine.ec2Inventory = inventory
	var got operatorv1.IAMQuiescence
	engine.OnQuiesced = func(_ context.Context, q operatorv1.IAMQuiescence) {
		if len(events) == 0 {
			assert.Equal(t, 0, reaper.deletesAttempted, "no stage may have run yet")
		}
		events = append(events, "quiesced")
		assert.Equal(t, AccountStatusTerminating, accounts.account.Status)
		got = q
	}
	engine.OnStage = func(context.Context, StageResult) {
		if len(events) == 0 || events[len(events)-1] != "stage" {
			events = append(events, "stage")
		}
	}

	req := request()
	req.OperationID, req.Fence = "op-7", 3
	_, err := engine.Teardown(context.Background(), req)

	require.NoError(t, err)
	assert.Equal(t, []string{"quiesced", "stage", "quiesced", "stage"}, events, "the second evaluation precedes the platform stage")
	assert.Equal(t, "op-7", got.OperationID)
	assert.Equal(t, uint64(3), got.Fence)
	assert.Equal(t, uint64(1), got.Generation)
	assert.Equal(t, 3, inventory.calls, "two evaluations and the final re-read")
	assert.True(t, accounts.deleted)
}

// A fence the read-back does not confirm must not spend an inventory call, and
// the convergence loop must not run a pass behind a fence it cannot read.
func TestTeardownRecordsAFailedReadbackAndRunsNoPassBehindIt(t *testing.T) {
	accounts := newFakeAccounts()
	engine := quietEngine(accounts)
	inventory := &stubInventory{reply: *completeInventory()}
	engine.ec2Inventory = inventory
	var got operatorv1.IAMQuiescence
	engine.OnQuiesced = func(_ context.Context, q operatorv1.IAMQuiescence) { got = q }
	accounts.getErr = nil

	engine.Accounts = &readbackFails{fakeAccounts: accounts}
	req := request()
	req.OperationID, req.Fence = "op-1", 1
	_, err := engine.Teardown(context.Background(), req)

	require.Error(t, err)
	require.Len(t, got.Unresolved, 1)
	assert.Equal(t, operatorv1.ReasonIAMReadbackFailed, got.Unresolved[0].Reason)
	assert.Equal(t, 0, inventory.calls, "no evaluation reads inventory behind an unconfirmed fence")
	assert.False(t, accounts.deleted)
}

// readbackFails serves the pre-quiesce read and fails the one after it.
type readbackFails struct {
	*fakeAccounts

	reads int
}

func (r *readbackFails) GetAccount(id string) (*Account, error) {
	r.reads++
	if r.reads > 1 {
		return nil, errors.New("kv down")
	}
	return r.fakeAccounts.GetAccount(id)
}
