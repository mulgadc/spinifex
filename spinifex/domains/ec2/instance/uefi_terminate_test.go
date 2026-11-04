package instance

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	vmmock "github.com/mulgadc/spinifex/spinifex/runtime/compute/vm/mock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const uefiTerminateID = "i-0123456789abcdef0"

// stoppedUEFIFixture is a stopped instance with a live store record and three
// retained objects, the state a launch and a few stops leave.
func stoppedUEFIFixture(t *testing.T) (*InstanceServiceImpl, *vmmock.StateStore, *uefistore.Store, *objectstore.MemoryObjectStore) {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	stores := uefistore.NewStore(js)
	require.NoError(t, stores.Open(t.Context()))
	_, err := stores.Create(t.Context(), &uefistore.UEFIStoreRecord{InstanceID: uefiTerminateID, AccountID: "acc", State: uefistore.StateLive})
	require.NoError(t, err)

	objects := objectstore.NewMemoryObjectStore()
	for _, k := range []string{"a", "b", "c"} {
		_, err := objects.PutObject(t.Context(), &s3.PutObjectInput{
			Bucket: aws.String("spinifex"), Key: aws.String(uefistore.KeyPrefix(uefiTerminateID) + k), Body: bytes.NewReader([]byte("vars")),
		})
		require.NoError(t, err)
	}

	store := &vmmock.StateStore{Stopped: map[string]*vm.VM{
		uefiTerminateID: {ID: uefiTerminateID, Status: vm.StateStopped, AccountID: "acc"},
	}}
	svc := &InstanceServiceImpl{stoppedStore: store, objectStore: objects}
	svc.SetUEFIStores(stores)
	return svc, store, stores, objects
}

func storeState(t *testing.T, stores *uefistore.Store) string {
	t.Helper()
	rec, _, err := stores.Get(t.Context(), uefiTerminateID)
	require.NoError(t, err)
	return rec.State
}

// TestS8_TerminateStoppedInstanceRecordsTheUEFIStoreObligationAndReturns
// enforces ADR-0003:S8: the request deletes no store object, it records the
// obligation on the store record and the terminated record, and returns.
func TestS8_TerminateStoppedInstanceRecordsTheUEFIStoreObligationAndReturns(t *testing.T) {
	svc, store, stores, objects := stoppedUEFIFixture(t)

	_, err := svc.TerminateStoppedInstance(t.Context(), &TerminateStoppedInstanceInput{InstanceID: uefiTerminateID}, "acc")
	require.NoError(t, err)

	keys, err := uefistore.ListObjects(t.Context(), objects, "spinifex", uefiTerminateID)
	require.NoError(t, err)
	assert.Len(t, keys, 3, "ADR-0003:S8: the request must leave the object deletion to the reaper")
	assert.Equal(t, uefistore.StateDeleting, storeState(t, stores), "the store record carries the obligation")
	require.NotNil(t, store.WroteTerminated[uefiTerminateID])
	assert.Equal(t, string(vm.TeardownPending), store.WroteTerminated[uefiTerminateID].Teardown[vm.TeardownUEFI])
}

// TestS7_FailedStoppedTerminateKeepsTheUEFIStoreLive enforces ADR-0003:S7: a
// terminate that fails before it is durable leaves the instance stopped, so
// its store must stay live rather than be handed to the reaper.
func TestS7_FailedStoppedTerminateKeepsTheUEFIStoreLive(t *testing.T) {
	svc, store, stores, _ := stoppedUEFIFixture(t)
	store.WriteTerminatedErr = errors.New("kv write boom")

	_, err := svc.TerminateStoppedInstance(t.Context(), &TerminateStoppedInstanceInput{InstanceID: uefiTerminateID}, "acc")
	require.Error(t, err)

	assert.Equal(t, uefistore.StateLive, storeState(t, stores),
		"ADR-0003:S7: a stopped instance whose terminate failed must keep its store")
}

func TestTerminateStoppedInstance_NoUEFIStoreStampsNoEntry(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	stores := uefistore.NewStore(js)
	require.NoError(t, stores.Open(t.Context()))
	store := &vmmock.StateStore{Stopped: map[string]*vm.VM{
		uefiTerminateID: {ID: uefiTerminateID, Status: vm.StateStopped, AccountID: "acc"},
	}}
	svc := &InstanceServiceImpl{stoppedStore: store}
	svc.SetUEFIStores(stores)

	_, err := svc.TerminateStoppedInstance(t.Context(), &TerminateStoppedInstanceInput{InstanceID: uefiTerminateID}, "acc")
	require.NoError(t, err)

	_, stamped := store.WroteTerminated[uefiTerminateID].Teardown[vm.TeardownUEFI]
	assert.False(t, stamped)
}

type unreadableUEFIStores struct{ marked bool }

func (u *unreadableUEFIStores) Exists(context.Context, string) (bool, error) {
	return false, errors.New("kv unavailable")
}

func (u *unreadableUEFIStores) MarkDeleting(context.Context, string) (bool, error) {
	u.marked = true
	return false, nil
}

// An unreadable bucket does not fail the terminate; the entry is stamped
// failed so the store reaper settles it from the terminated record.
func TestTerminateStoppedInstance_UnreadableUEFIStoreIsStampedFailed(t *testing.T) {
	store := &vmmock.StateStore{Stopped: map[string]*vm.VM{
		uefiTerminateID: {ID: uefiTerminateID, Status: vm.StateStopped, AccountID: "acc"},
	}}
	stores := &unreadableUEFIStores{}
	svc := &InstanceServiceImpl{stoppedStore: store}
	svc.SetUEFIStores(stores)

	_, err := svc.TerminateStoppedInstance(t.Context(), &TerminateStoppedInstanceInput{InstanceID: uefiTerminateID}, "acc")
	require.NoError(t, err)

	assert.Equal(t, string(vm.TeardownFailed), store.WroteTerminated[uefiTerminateID].Teardown[vm.TeardownUEFI])
	assert.False(t, stores.marked)
}
