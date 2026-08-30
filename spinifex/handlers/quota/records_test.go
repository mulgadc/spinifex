package handlers_quota_test

import (
	"encoding/json"
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
