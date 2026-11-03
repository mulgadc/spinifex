package accountlifecycle

//test:in-package — the exercise's inventory and cleanup collaborators are
// unexported seams, and the S7 invariants drive the engine directly.

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
	operatorv1 "github.com/mulgadc/spinifex/contracts/operator/v1"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/accountcleanup"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubCleaner struct {
	calls  []accountcleanup.Request
	result *accountcleanup.Result
	err    error
	// onCall runs inside the cleanup, so a test can observe what was recorded first.
	onCall func()
}

func (s *stubCleaner) CleanupPlatform(_ context.Context, req *accountcleanup.Request) (*accountcleanup.Result, error) {
	s.calls = append(s.calls, *req)
	if s.onCall != nil {
		s.onCall()
	}
	return s.result, s.err
}

// sequencedInventory answers the quiescence evaluation first, then the re-read.
type sequencedInventory struct {
	replies []ec2v1.PlatformInventoryReply
	calls   int
}

func (s *sequencedInventory) PlatformInventory(context.Context, string, string) ec2v1.PlatformInventoryReply {
	reply := s.replies[min(s.calls, len(s.replies)-1)]
	s.calls++
	return reply
}

func exerciseEngine(accounts AccountStore, cleaner PlatformCleaner, inventory ec2PlatformInventory) *Engine {
	engine := quietEngine(accounts)
	engine.Cleanup = cleaner
	engine.ec2Inventory = inventory
	return engine
}

func exerciseRequest() Request {
	req := request()
	req.OperationID, req.Fence = "op-1", 1
	return req
}

func cleanResult() *accountcleanup.Result {
	res := &accountcleanup.Result{AccountID: testAccountID}
	for _, kind := range ec2v1.PlatformKinds() {
		res.Kinds = append(res.Kinds, accountcleanup.KindReport{Kind: kind, Status: ec2v1.PlatformStatusObservedClean})
	}
	return res
}

func accountingFor(result *operatorv1.ExerciseResult, kind string) operatorv1.KindAccounting {
	for _, entry := range result.Accounting {
		if entry.Kind == kind {
			return entry
		}
	}
	return operatorv1.KindAccounting{}
}

// The exercise records the evaluation before it cleans, cleans only the kinds that
// evaluation recorded as quiesced, and never removes the account record.
func TestExerciseCleansTheQuiescedKindsAfterRecordingAndKeepsTheAccount(t *testing.T) {
	accounts := newFakeAccounts()
	var recorded *operatorv1.IAMQuiescence
	cleaner := &stubCleaner{result: cleanResult()}
	cleaner.onCall = func() { assert.NotNil(t, recorded, "quiescence must be recorded before any cleanup") }
	inventory := &sequencedInventory{replies: []ec2v1.PlatformInventoryReply{*completeInventory()}}
	engine := exerciseEngine(accounts, cleaner, inventory)
	engine.OnQuiesced = func(_ context.Context, q operatorv1.IAMQuiescence) { recorded = &q }

	result, err := engine.Exercise(context.Background(), exerciseRequest())

	require.NoError(t, err)
	require.NotNil(t, recorded)
	assert.ElementsMatch(t, ec2PlatformKinds, recorded.QuiescedScope)
	require.Len(t, cleaner.calls, 1)
	assert.ElementsMatch(t, recorded.QuiescedScope, cleaner.calls[0].Kinds)
	assert.Equal(t, "op-1", cleaner.calls[0].CorrelationRef)
	assert.False(t, accounts.deleted, "an exercise never removes the account record")
	assert.Equal(t, AccountStatusTerminating, accounts.account.Status)
	assert.Equal(t, 2, inventory.calls, "one evaluation read and one final re-read")
	for _, kind := range recorded.QuiescedScope {
		assert.Equal(t, operatorv1.AccountingAccounted, accountingFor(result, kind).Status, kind)
	}
}

// The final re-read is held against the cleanup result and the quiescence record
// is not rewritten from it.
func TestExerciseAccountsForARemainderOnlyWhenTheCleanupNamedIt(t *testing.T) {
	cases := map[string]struct {
		cleanup *accountcleanup.Result
		reread  ec2v1.PlatformInventoryReply
		want    string
		reason  string
	}{
		"late write the cleanup never saw": {
			cleanup: cleanResult(),
			reread:  rereadWithKeyPair("key-late"),
			want:    operatorv1.AccountingUnaccounted, reason: operatorv1.AccountingReasonUnnamedRemainder,
		},
		"cleanup deleted it but it is back": {
			cleanup: withItem(cleanResult(), accountcleanup.Item{ResourceID: "key-1", Kind: ec2v1.PlatformKindKeyPair, Disposition: accountcleanup.DispositionDeleted}),
			reread:  rereadWithKeyPair("key-1"),
			want:    operatorv1.AccountingUnaccounted, reason: operatorv1.AccountingReasonUnnamedRemainder,
		},
		"cleanup blocked it with a reason": {
			cleanup: withItem(cleanResult(), accountcleanup.Item{ResourceID: "key-1", Kind: ec2v1.PlatformKindKeyPair, Disposition: accountcleanup.DispositionBlocked, Reason: accountcleanup.ReasonInstanceReference}),
			reread:  rereadWithKeyPair("key-1"),
			want:    operatorv1.AccountingAccounted,
		},
		"re-read incomplete": {
			cleanup: cleanResult(),
			reread:  *incompleteKeyPairs(),
			want:    operatorv1.AccountingUnaccounted, reason: operatorv1.AccountingReasonRereadIncomplete,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			accounts := newFakeAccounts()
			inventory := &sequencedInventory{replies: []ec2v1.PlatformInventoryReply{*completeInventory(), tc.reread}}
			engine := exerciseEngine(accounts, &stubCleaner{result: tc.cleanup}, inventory)
			var recorded operatorv1.IAMQuiescence
			engine.OnQuiesced = func(_ context.Context, q operatorv1.IAMQuiescence) { recorded = q }

			result, err := engine.Exercise(context.Background(), exerciseRequest())

			require.NoError(t, err)
			entry := accountingFor(result, ec2v1.PlatformKindKeyPair)
			assert.Equal(t, tc.want, entry.Status)
			assert.Equal(t, tc.reason, entry.Reason)
			assert.Contains(t, recorded.QuiescedScope, ec2v1.PlatformKindKeyPair, "quiescence is not rewritten from the re-read")
		})
	}
}

func rereadWithKeyPair(id string) ec2v1.PlatformInventoryReply {
	reply := *completeInventory()
	reply.Kinds[0].Status = ec2v1.PlatformStatusRecordsFound
	reply.Items = []ec2v1.PlatformItem{{ResourceID: id, Kind: ec2v1.PlatformKindKeyPair, Disposition: accountcleanup.DispositionBlocked}}
	return reply
}

func incompleteKeyPairs() *ec2v1.PlatformInventoryReply {
	reply := completeInventory()
	reply.Kinds[0].Status = ec2v1.PlatformStatusScanIncomplete
	return reply
}

func withItem(res *accountcleanup.Result, item accountcleanup.Item) *accountcleanup.Result {
	res.Items = append(res.Items, item)
	return res
}

// With no kind quiesced the exercise runs no cleanup and records why.
func TestExerciseWithNoQuiescedKindRunsNoCleanup(t *testing.T) {
	accounts := newFakeAccounts()
	cleaner := &stubCleaner{result: cleanResult()}
	incomplete := ec2v1.IncompletePlatformInventory(testAccountID, "op-1", ec2v1.PlatformReasonTimeout)
	engine := exerciseEngine(accounts, cleaner, &sequencedInventory{replies: []ec2v1.PlatformInventoryReply{incomplete}})

	result, err := engine.Exercise(context.Background(), exerciseRequest())

	require.NoError(t, err)
	assert.Empty(t, cleaner.calls)
	assert.Equal(t, operatorv1.ExerciseSkippedNoQuiescedKind, result.Skipped)
	assert.Nil(t, result.Cleanup)
}

func TestExerciseRefusesForceAndAMissingCollaborator(t *testing.T) {
	accounts := newFakeAccounts()
	req := exerciseRequest()
	req.Force = true
	_, err := exerciseEngine(accounts, &stubCleaner{}, &stubInventory{}).Exercise(context.Background(), req)
	require.Error(t, err)

	_, err = exerciseEngine(accounts, nil, &stubInventory{}).Exercise(context.Background(), exerciseRequest())
	require.Error(t, err)
	assert.Empty(t, accounts.quiesces, "a refused exercise quiesces nothing")
}

// ADR-0003:S7: the exercise is an automated actor that deletes customer-visible
// records, so it acts only on kinds whose quiescence was recorded under a confirmed
// fence, and a failed cleanup leaves the account where it is.
func TestS7_ExerciseCleansNothingUnlessItsQuiescenceWasRecordedUnderAConfirmedFence(t *testing.T) {
	cases := map[string]func(*Engine, *fakeAccounts) *sequencedInventory{
		"read-back fails": func(e *Engine, a *fakeAccounts) *sequencedInventory {
			e.Accounts = &readbackFails{fakeAccounts: a}
			return &sequencedInventory{replies: []ec2v1.PlatformInventoryReply{*completeInventory()}}
		},
		"inventory incomplete": func(*Engine, *fakeAccounts) *sequencedInventory {
			return &sequencedInventory{replies: []ec2v1.PlatformInventoryReply{
				ec2v1.IncompletePlatformInventory(testAccountID, "op-1", ec2v1.PlatformReasonNoResponder),
			}}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			accounts := newFakeAccounts()
			cleaner := &stubCleaner{result: cleanResult()}
			engine := exerciseEngine(accounts, cleaner, nil)
			engine.ec2Inventory = setup(engine, accounts)

			_, err := engine.Exercise(context.Background(), exerciseRequest())

			require.NoError(t, err)
			assert.Empty(t, cleaner.calls, "no kind may be cleaned without a recorded quiescence")
			assert.False(t, accounts.deleted)
		})
	}

	t.Run("only the kinds with a complete inventory are cleaned", func(t *testing.T) {
		accounts := newFakeAccounts()
		cleaner := &stubCleaner{result: cleanResult()}
		partial := completeInventory()
		partial.Kinds[1].Status = ec2v1.PlatformStatusScanIncomplete
		engine := exerciseEngine(accounts, cleaner, &sequencedInventory{replies: []ec2v1.PlatformInventoryReply{*partial}})

		_, err := engine.Exercise(context.Background(), exerciseRequest())

		require.NoError(t, err)
		require.Len(t, cleaner.calls, 1)
		assert.ElementsMatch(t, []string{ec2v1.PlatformKindKeyPair, ec2v1.PlatformKindLaunchTemplate}, cleaner.calls[0].Kinds)
	})

	t.Run("a failed cleanup returns an error and removes nothing", func(t *testing.T) {
		accounts := newFakeAccounts()
		engine := exerciseEngine(accounts, &stubCleaner{err: errors.New("owner down")}, &sequencedInventory{replies: []ec2v1.PlatformInventoryReply{*completeInventory()}})

		_, err := engine.Exercise(context.Background(), exerciseRequest())

		require.Error(t, err)
		assert.False(t, accounts.deleted)
	})
}

func ordinaryEngine(accounts AccountStore, cleaner PlatformCleaner, replies ...ec2v1.PlatformInventoryReply) *Engine {
	return exerciseEngine(accounts, cleaner, &sequencedInventory{replies: replies})
}

// ADR-0003:S7: an ordinary delete cleans a kind only after recording it quiesced
// under the fence, and cleans only the recorded kinds.
func TestS7_OrdinaryDeleteCleansOnlyKindsItRecordedQuiesced(t *testing.T) {
	accounts := newFakeAccounts()
	var recorded []operatorv1.IAMQuiescence
	cleaner := &stubCleaner{result: cleanResult()}
	cleaner.onCall = func() { assert.NotEmpty(t, recorded, "quiescence must be recorded before any cleanup") }
	partial := *completeInventory()
	partial.Kinds[0].Status = ec2v1.PlatformStatusScanIncomplete
	engine := ordinaryEngine(accounts, cleaner, *completeInventory(), partial, *completeInventory())
	engine.OnQuiesced = func(_ context.Context, q operatorv1.IAMQuiescence) { recorded = append(recorded, q) }

	_, err := engine.Teardown(context.Background(), exerciseRequest())

	require.NoError(t, err)
	require.Len(t, recorded, 2)
	require.Len(t, cleaner.calls, 1)
	assert.ElementsMatch(t, recorded[1].QuiescedScope, cleaner.calls[0].Kinds)
	assert.NotContains(t, cleaner.calls[0].Kinds, ec2v1.PlatformKindKeyPair, "an incomplete kind is never cleaned")
	assert.True(t, cleaner.calls[0].DeleteLiveTemplates)
}

// ADR-0003:S7: with no kind quiesced the cleanup does not run, and a final
// re-read that cannot complete keeps the account TERMINATING.
func TestS7_OrdinaryDeleteWithNoQuiescedKindCleansNothingAndKeepsTheAccount(t *testing.T) {
	accounts := newFakeAccounts()
	cleaner := &stubCleaner{result: cleanResult()}
	incomplete := ec2v1.IncompletePlatformInventory(testAccountID, "op-1", ec2v1.PlatformReasonTimeout)
	engine := ordinaryEngine(accounts, cleaner, incomplete)

	result, err := engine.Teardown(context.Background(), exerciseRequest())

	require.ErrorIs(t, err, ErrResourcesStuck)
	assert.Empty(t, cleaner.calls)
	assert.False(t, accounts.deleted)
	assert.Positive(t, result.StuckCount())
	assert.Equal(t, AccountStatusTerminating, accounts.account.Status)
}

// ADR-0003:S7: a record that remains after cleanup keeps the account, whatever
// Force says, and is named with the cleanup's own reason.
func TestS7_OrdinaryDeleteKeepsTheAccountWhileAnL09RecordRemains(t *testing.T) {
	for _, force := range []bool{false, true} {
		accounts := newFakeAccounts()
		cleaner := &stubCleaner{result: withItem(cleanResult(), accountcleanup.Item{
			ResourceID: "key-1", Kind: ec2v1.PlatformKindKeyPair, Disposition: accountcleanup.DispositionBlocked, Reason: accountcleanup.ReasonInstanceReference,
		})}
		engine := ordinaryEngine(accounts, cleaner, *completeInventory(), rereadWithKeyPair("key-1"))
		req := exerciseRequest()
		req.Force = force

		result, err := engine.Teardown(context.Background(), req)

		require.ErrorIs(t, err, ErrResourcesStuck, "force=%v", force)
		assert.False(t, accounts.deleted, "force=%v", force)
		var reasons []string
		for _, stage := range result.Stages {
			for _, stuck := range stage.Stuck {
				reasons = append(reasons, stuck.Reason)
			}
		}
		assert.Contains(t, reasons, accountcleanup.ReasonInstanceReference)
	}
}

// ADR-0003:S7: without the cleanup or the inventory an ordinary delete does not
// start, so it cannot fall back to deleting the account with L09 records unread.
func TestS7_OrdinaryDeleteRefusesToStartWithoutTheCleanupOrInventory(t *testing.T) {
	accounts := newFakeAccounts()
	engine := ordinaryEngine(accounts, nil, *completeInventory())
	engine.Cleanup = nil

	_, err := engine.Teardown(context.Background(), exerciseRequest())

	require.ErrorIs(t, err, errPlatformCleanupMissing)
	assert.False(t, accounts.deleted)
	assert.NotEqual(t, AccountStatusTerminating, accounts.account.Status, "nothing was quiesced")
}

func TestInstanceReferencesSkipTerminalInstancesAndNameKeysAndGroups(t *testing.T) {
	refs := instanceReferences(&ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{{Instances: []*ec2.Instance{
		instanceWith("running", "kp-a", "pg-a", "pg-1"),
		instanceWith("stopped", "kp-b", "", ""),
		instanceWith("terminated", "kp-gone", "pg-gone", "pg-9"),
	}}}})

	assert.Equal(t, map[string]bool{"kp-a": true, "kp-b": true}, refs.KeyNames)
	assert.Equal(t, map[string]bool{"pg-a": true}, refs.PlacementGroupNames)
	assert.Equal(t, map[string]bool{"pg-1": true}, refs.PlacementGroupIDs)
}

func instanceWith(state, key, groupName, groupID string) *ec2.Instance {
	return &ec2.Instance{
		State:     &ec2.InstanceState{Name: aws.String(state)},
		KeyName:   aws.String(key),
		Placement: &ec2.Placement{GroupName: aws.String(groupName), GroupId: aws.String(groupID)},
	}
}
