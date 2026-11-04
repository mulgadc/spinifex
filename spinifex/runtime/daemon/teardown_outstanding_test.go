package daemon

import (
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	ec2instance "github.com/mulgadc/spinifex/spinifex/domains/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	teardownAcct   = "777700000001"
	teardownOtherA = "777700000002"
)

// teardownReadFixture builds the instance service over a real JetStream of its
// own, so a record written here never reaches another test's listing.
func teardownReadFixture(t *testing.T) (*JetStreamManager, *ec2instance.InstanceServiceImpl) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	m, err := NewJetStreamManager(nc)
	require.NoError(t, err)
	require.NoError(t, m.InitKVBucket())
	require.NoError(t, m.InitTerminatedInstanceBucket())
	svc := ec2instance.NewInstanceServiceImpl(nil, nil, nc, objectstore.NewMemoryObjectStore(), nil, nil, m)
	return m, svc
}

func liveRecord(t *testing.T, m *JetStreamManager, id, account string, status vm.InstanceState, teardown map[string]string) {
	t.Helper()
	v := &vm.VM{ID: id, AccountID: account, Status: status, Teardown: teardown}
	require.NoError(t, m.WriteInstanceRecord(id, v.Record()))
}

func terminatedRecord(t *testing.T, m *JetStreamManager, id, account string, teardown map[string]string) {
	t.Helper()
	v := &vm.VM{ID: id, AccountID: account, Status: vm.StateTerminated, Teardown: teardown}
	require.NoError(t, m.WriteTerminatedInstance(id, v))
}

func outstandingStates(t *testing.T, svc *ec2instance.InstanceServiceImpl, account string) map[string]string {
	t.Helper()
	out, err := svc.AccountTeardownOutstanding(account)
	require.NoError(t, err)
	states := map[string]string{}
	for _, o := range out {
		states[o.InstanceID] = o.State
	}
	return states
}

var teardownAllDone = map[string]string{
	vm.TeardownQEMU:    string(vm.TeardownDone),
	vm.TeardownVolumes: string(vm.TeardownDone),
}

// Only a terminated record whose every dependent is done is torn down. A live
// record with an empty Teardown map has recorded no progress and proves nothing.
func TestAccountTeardownOutstanding_OnlyTerminatedAndCompleteIsOmitted(t *testing.T) {
	m, svc := teardownReadFixture(t)
	liveRecord(t, m, "i-running", teardownAcct, vm.StateRunning, nil)
	liveRecord(t, m, "i-stopped", teardownAcct, vm.StateStopped, nil)
	liveRecord(t, m, "i-shutting", teardownAcct, vm.StateShuttingDown, map[string]string{})
	terminatedRecord(t, m, "i-failed-dep", teardownAcct, map[string]string{
		vm.TeardownQEMU:    string(vm.TeardownDone),
		vm.TeardownVolumes: string(vm.TeardownFailed),
	})
	terminatedRecord(t, m, "i-pending-dep", teardownAcct, map[string]string{
		vm.TeardownQEMU: string(vm.TeardownDone),
		vm.TeardownUEFI: string(vm.TeardownPending),
	})
	terminatedRecord(t, m, "i-complete", teardownAcct, teardownAllDone)

	assert.Equal(t, map[string]string{
		"i-running":     string(vm.StateRunning),
		"i-stopped":     string(vm.StateStopped),
		"i-shutting":    string(vm.StateShuttingDown),
		"i-failed-dep":  string(vm.StateTerminated),
		"i-pending-dep": string(vm.StateTerminated),
	}, outstandingStates(t, svc, teardownAcct))
}

// The dependents are returned, so a caller can say what is still outstanding.
func TestAccountTeardownOutstanding_ReportsPerDependentState(t *testing.T) {
	m, svc := teardownReadFixture(t)
	terminatedRecord(t, m, "i-failed-dep", teardownAcct, map[string]string{
		vm.TeardownQEMU:    string(vm.TeardownDone),
		vm.TeardownVolumes: string(vm.TeardownFailed),
	})

	out, err := svc.AccountTeardownOutstanding(teardownAcct)

	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, map[string]string{"qemu": "done", "volumes": "failed"}, out[0].Teardown)
}

// A terminated record that recorded no dependent at all is complete by
// TeardownComplete, so it is omitted. This pins that reading rather than
// endorsing it.
func TestAccountTeardownOutstanding_TerminatedWithNoRecordedDependentIsOmitted(t *testing.T) {
	m, svc := teardownReadFixture(t)
	terminatedRecord(t, m, "i-no-deps", teardownAcct, nil)

	assert.Empty(t, outstandingStates(t, svc, teardownAcct))
}

// Another account's instances are never reported, nor is a pre-account record
// that only the global account may see.
func TestAccountTeardownOutstanding_OtherAccountsAreNotReported(t *testing.T) {
	m, svc := teardownReadFixture(t)
	liveRecord(t, m, "i-mine", teardownAcct, vm.StateRunning, nil)
	liveRecord(t, m, "i-theirs", teardownOtherA, vm.StateRunning, nil)
	liveRecord(t, m, "i-ownerless", "", vm.StateRunning, nil)
	terminatedRecord(t, m, "i-theirs-term", teardownOtherA, map[string]string{vm.TeardownQEMU: string(vm.TeardownFailed)})

	assert.Equal(t, map[string]string{"i-mine": string(vm.StateRunning)}, outstandingStates(t, svc, teardownAcct))
}

// A terminate in flight can leave a record in both stores. The live copy is
// what is reported, so a complete terminated copy cannot hide it.
func TestAccountTeardownOutstanding_LiveCopyWinsOverTerminatedCopy(t *testing.T) {
	m, svc := teardownReadFixture(t)
	liveRecord(t, m, "i-both", teardownAcct, vm.StateShuttingDown, nil)
	terminatedRecord(t, m, "i-both", teardownAcct, map[string]string{vm.TeardownQEMU: string(vm.TeardownFailed)})

	assert.Equal(t, map[string]string{"i-both": string(vm.StateShuttingDown)}, outstandingStates(t, svc, teardownAcct))
}

func TestAccountTeardownOutstanding_LiveCopyIsReportedEvenIfTerminatedCopyIsComplete(t *testing.T) {
	m, svc := teardownReadFixture(t)
	liveRecord(t, m, "i-both", teardownAcct, vm.StateRunning, nil)
	terminatedRecord(t, m, "i-both", teardownAcct, teardownAllDone)

	assert.Equal(t, map[string]string{"i-both": string(vm.StateRunning)}, outstandingStates(t, svc, teardownAcct))
}

func TestAccountTeardownOutstanding_NothingOutstandingIsEmptyNotAnError(t *testing.T) {
	_, svc := teardownReadFixture(t)

	out, err := svc.AccountTeardownOutstanding(teardownAcct)

	require.NoError(t, err)
	assert.Empty(t, out)
}

// A record that cannot be decoded might be the account's, so the read fails
// instead of returning a list that omits it.
func TestAccountTeardownOutstanding_UndecodableLiveRecordFailsTheRead(t *testing.T) {
	m, svc := teardownReadFixture(t)
	liveRecord(t, m, "i-good", teardownAcct, vm.StateRunning, nil)
	kv, err := m.stateB.KV(t.Context())
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), instanceRecordKey("i-corrupt"), []byte("{not json"))
	require.NoError(t, err)

	out, err := svc.AccountTeardownOutstanding(teardownAcct)

	require.Error(t, err)
	assert.Nil(t, out)
}

func TestAccountTeardownOutstanding_UndecodableTerminatedRecordFailsTheRead(t *testing.T) {
	m, svc := teardownReadFixture(t)
	terminatedRecord(t, m, "i-good", teardownAcct, teardownAllDone)
	kv, err := m.term.KV(t.Context())
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), instanceRecordKey("i-corrupt"), []byte("{not json"))
	require.NoError(t, err)

	out, err := svc.AccountTeardownOutstanding(teardownAcct)

	require.Error(t, err)
	assert.Nil(t, out)
}

// finalizeTerminated persists the terminated state to the primary store before
// the terminated bucket is written, and the second write can fail. A record
// with an unfinished dependent that sits only in the primary store must show.
func TestAccountTeardownOutstanding_TerminatedOnlyInPrimaryStoreIsReported(t *testing.T) {
	m, svc := teardownReadFixture(t)
	liveRecord(t, m, "i-gap", teardownAcct, vm.StateTerminated, map[string]string{
		vm.TeardownQEMU:    string(vm.TeardownDone),
		vm.TeardownVolumes: string(vm.TeardownPending),
	})
	liveRecord(t, m, "i-gap-done", teardownAcct, vm.StateTerminated, teardownAllDone)

	assert.Equal(t, map[string]string{"i-gap": string(vm.StateTerminated)}, outstandingStates(t, svc, teardownAcct))
}
