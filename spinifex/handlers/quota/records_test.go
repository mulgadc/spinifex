package handlers_quota_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	handlers_quota "github.com/mulgadc/spinifex/spinifex/handlers/quota"
	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/resource"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/require"
)

const recordPrefix = "i."

func recordStore(t *testing.T) *kvstore.Store[vm.InstanceRecord] {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	return kvstore.New[vm.InstanceRecord](testutil.NewJetStream(t, nc), kvstore.Config{
		Name:     "spinifex-instance-state",
		History:  1,
		Replicas: 1,
	})
}

func putRecord(t *testing.T, store *kvstore.Store[vm.InstanceRecord], id, accountID, instanceType string, state vm.InstanceState) {
	t.Helper()
	require.NoError(t, store.Set(t.Context(), recordPrefix+id, &vm.InstanceRecord{
		Metadata: resource.Metadata{Name: id, AccountID: accountID},
		Spec:     vm.InstanceSpec{InstanceType: instanceType},
		Status:   vm.InstanceStatus{Status: state},
	}))
}

// The sum is per account, from the record space, in one read.
func TestRecordVCPULister_TotalsPerAccount(t *testing.T) {
	store := recordStore(t)
	const a, b = "111111111111", "222222222222"
	putRecord(t, store, "i-1", a, "m5.xlarge", vm.StateRunning) // 4
	putRecord(t, store, "i-2", a, "t3.micro", vm.StateRunning)  // 2
	putRecord(t, store, "i-3", b, "m5.xlarge", vm.StateRunning) // 4

	totals, complete, err := handlers_quota.RecordVCPULister(store, recordPrefix)(t.Context())
	require.NoError(t, err)
	require.True(t, complete)
	require.Equal(t, map[string]int{a: 6, b: 4}, totals)
}

// A stopped instance still exists and is charged; only shutting-down and
// terminated leave the counted set, so a reaped instance frees quota.
func TestRecordVCPULister_ChargesStoppedButNotTerminal(t *testing.T) {
	store := recordStore(t)
	const account = "111111111111"
	putRecord(t, store, "i-1", account, "t3.micro", vm.StateRunning)       // 2
	putRecord(t, store, "i-2", account, "t3.micro", vm.StateStopped)       // 2
	putRecord(t, store, "i-3", account, "t3.micro", vm.StatePending)       // 2
	putRecord(t, store, "i-4", account, "m5.xlarge", vm.StateShuttingDown) // excluded
	putRecord(t, store, "i-5", account, "m5.xlarge", vm.StateTerminated)   // excluded

	totals, _, err := handlers_quota.RecordVCPULister(store, recordPrefix)(t.Context())
	require.NoError(t, err)
	require.Equal(t, map[string]int{account: 6}, totals)
}

// The system account is never charged, and an account holding nothing is absent
// rather than present as zero — the caller's account list is what zeroes it.
func TestRecordVCPULister_SkipsSystemAccountAndReportsNothingAsAbsent(t *testing.T) {
	store := recordStore(t)
	putRecord(t, store, "i-1", utils.GlobalAccountID, "m5.xlarge", vm.StateRunning)

	totals, complete, err := handlers_quota.RecordVCPULister(store, recordPrefix)(t.Context())
	require.NoError(t, err)
	require.True(t, complete)
	require.Empty(t, totals)
}

// An unknown instance type contributes nothing rather than failing the sweep,
// matching the sum the fan-out produced.
func TestRecordVCPULister_UnknownInstanceTypeContributesNothing(t *testing.T) {
	store := recordStore(t)
	const account = "111111111111"
	putRecord(t, store, "i-1", account, "not-a-real-type", vm.StateRunning)
	putRecord(t, store, "i-2", account, "t3.micro", vm.StateRunning)

	totals, _, err := handlers_quota.RecordVCPULister(store, recordPrefix)(t.Context())
	require.NoError(t, err)
	require.Equal(t, map[string]int{account: 2}, totals)
}

// stubSource stands in for the record store so a snapshot can be made
// deliberately stale, which a single-node server never does on its own.
type stubSource struct {
	items         []kvstore.Item[vm.InstanceRecord]
	highWater     uint64
	watermark     uint64
	watermarkErr  error
	snapshotCalls int
}

func (s *stubSource) LastSequence(context.Context, string) (uint64, error) {
	return s.watermark, s.watermarkErr
}

func (s *stubSource) Snapshot(context.Context, string) ([]kvstore.Item[vm.InstanceRecord], uint64, error) {
	s.snapshotCalls++
	return s.items, s.highWater, nil
}

func stubItem(id, accountID, instanceType string, state vm.InstanceState) kvstore.Item[vm.InstanceRecord] {
	return kvstore.Item[vm.InstanceRecord]{
		Key:      recordPrefix + id,
		Revision: 1,
		Value: vm.InstanceRecord{
			Metadata: resource.Metadata{Name: id, AccountID: accountID},
			Spec:     vm.InstanceSpec{InstanceType: instanceType},
			Status:   vm.InstanceStatus{Status: state},
		},
	}
}

// A snapshot that reached the watermark saw every write that had completed
// when the pass began, so it is good enough to lower a counter.
func TestRecordVCPULister_CompleteWhenTheSnapshotReachesTheWatermark(t *testing.T) {
	const account = "111111111111"
	source := &stubSource{
		items:     []kvstore.Item[vm.InstanceRecord]{stubItem("i-1", account, "m5.xlarge", vm.StateRunning)},
		highWater: 12,
		watermark: 12,
	}

	totals, complete, err := handlers_quota.RecordVCPULister(source, recordPrefix)(t.Context())
	require.NoError(t, err)
	require.True(t, complete)
	require.Equal(t, map[string]int{account: 4}, totals)
	require.Equal(t, 1, source.snapshotCalls)
}

// A snapshot behind the watermark was served by a replica that has not caught
// up. The totals are still returned so the pass can raise; complete is false
// so it cannot lower and undo a charge for an instance it never saw.
func TestRecordVCPULister_IncompleteWhenTheSnapshotIsBehindTheWatermark(t *testing.T) {
	const account = "111111111111"
	source := &stubSource{
		items:     []kvstore.Item[vm.InstanceRecord]{stubItem("i-1", account, "m5.xlarge", vm.StateRunning)},
		highWater: 11,
		watermark: 12,
	}

	totals, complete, err := handlers_quota.RecordVCPULister(source, recordPrefix)(t.Context())
	require.NoError(t, err)
	require.False(t, complete)
	require.Equal(t, map[string]int{account: 4}, totals)
}

// A watermark that cannot be read is not a reason to fail the pass, but it is
// a reason not to trust the snapshot's currency.
func TestRecordVCPULister_IncompleteWhenTheWatermarkIsUnavailable(t *testing.T) {
	const account = "111111111111"
	source := &stubSource{
		items:        []kvstore.Item[vm.InstanceRecord]{stubItem("i-1", account, "t3.micro", vm.StateRunning)},
		highWater:    12,
		watermarkErr: errors.New("stream leader unavailable"),
	}

	totals, complete, err := handlers_quota.RecordVCPULister(source, recordPrefix)(t.Context())
	require.NoError(t, err)
	require.False(t, complete)
	require.Equal(t, map[string]int{account: 2}, totals)
}

// The live pairing is what makes complete reachable at all: against a real
// bucket the snapshot and the watermark agree, including when the newest
// event is the delete that releases the quota.
func TestRecordVCPULister_CompleteAgainstALiveBucketAfterADelete(t *testing.T) {
	store := recordStore(t)
	const account = "111111111111"
	putRecord(t, store, "i-1", account, "m5.xlarge", vm.StateRunning)
	putRecord(t, store, "i-2", account, "t3.micro", vm.StateRunning)
	require.NoError(t, store.Delete(t.Context(), recordPrefix+"i-2"))

	totals, complete, err := handlers_quota.RecordVCPULister(store, recordPrefix)(t.Context())
	require.NoError(t, err)
	require.True(t, complete)
	require.Equal(t, map[string]int{account: 4}, totals)
}

func TestAccountForRecord(t *testing.T) {
	valid, err := json.Marshal(vm.InstanceRecord{
		Metadata: resource.Metadata{Name: "i-1", AccountID: "111111111111"},
	})
	require.NoError(t, err)
	system, err := json.Marshal(vm.InstanceRecord{
		Metadata: resource.Metadata{Name: "i-2", AccountID: utils.GlobalAccountID},
	})
	require.NoError(t, err)
	unowned, err := json.Marshal(vm.InstanceRecord{Metadata: resource.Metadata{Name: "i-3"}})
	require.NoError(t, err)

	for _, tc := range []struct {
		name   string
		value  []byte
		want   string
		wantOK bool
	}{
		{"a record names its account", valid, "111111111111", true},
		// The system account is never charged, so attributing to it would queue
		// work that reconcile then skips.
		{"the system account is not a unit of work", system, "", false},
		{"a record with no account cannot be attributed", unowned, "", false},
		// A delete tombstone carries no value, which is the case the fallback
		// to a whole-set pass exists for.
		{"an empty value cannot be attributed", nil, "", false},
		{"a corrupt value cannot be attributed", []byte("{not json"), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := handlers_quota.AccountForRecord(tc.value)
			require.Equal(t, tc.wantOK, ok)
			require.Equal(t, tc.want, got)
		})
	}
}
