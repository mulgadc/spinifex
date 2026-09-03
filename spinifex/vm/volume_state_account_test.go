// These tests reuse fakeVolumeStateUpdater, newMockQMPClient and qmpRecorder,
// all package-internal.
//
//test:in-package
package vm

import (
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/qmp"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stateAccountID is the instance-owning account these tests seed. It is a real
// account ID rather than a placeholder because the production key builder
// rejects anything utils.IsAccountID refuses.
const stateAccountID = "000000000042"

// Volume documents are keyed under the owning account, so UpdateVolumeState's
// leading argument is a key segment rather than a label. The five production
// call sites all pass instance.AccountID; passing anything else — the instance
// ID is the very next argument and is also an untyped string — builds a key
// that does not exist and leaves the attachment record stranded. These tests
// are the only thing asserting that argument.

// TestAttachVolume_KeysStateOnTheInstanceAccount covers vm/volumes.go:190.
func TestAttachVolume_KeysStateOnTheInstanceAccount(t *testing.T) {
	qmpClient, cancel := newMockQMPClient(t, func(cmd qmp.QMPCommand) map[string]any { return nil })
	defer cancel()

	stateUpdater := &fakeVolumeStateUpdater{}
	mounter := &fakeVolumeMounter{mountOneURI: "nbd:unix:/tmp/test.sock"}
	m := NewManagerWithDeps(Deps{VolumeMounter: mounter, VolumeStateUpdater: stateUpdater})
	m.Insert(&VM{
		ID: "i-1", Status: StateRunning, Instance: &ec2.Instance{},
		QMPClient: qmpClient, AccountID: stateAccountID,
	})

	_, err := m.AttachVolume(t.Context(), "i-1", "vol-1", "/dev/sdf")
	require.NoError(t, err)

	calls := stateUpdater.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, stateAccountID, calls[0].AccountID,
		"the in-use write must be keyed on the instance's own account")
	assert.Equal(t, "in-use", calls[0].State)
}

// TestAttachVolume_RollbackKeysStateOnTheInstanceAccount covers the rollback at
// vm/volumes.go:213, which runs after device_add fails and is the path that
// would silently leave a volume stuck in-use if it wrote to the wrong key.
func TestAttachVolume_RollbackKeysStateOnTheInstanceAccount(t *testing.T) {
	qmpClient, cancel := newMockQMPClient(t, func(cmd qmp.QMPCommand) map[string]any {
		switch cmd.Execute {
		case "query-block":
			return map[string]any{"return": []qmp.BlockDevice{}}
		case "device_add":
			return map[string]any{"error": map[string]any{"class": "GenericError", "desc": "no PCI slot"}}
		}
		return nil
	})
	defer cancel()

	stateUpdater := &fakeVolumeStateUpdater{}
	mounter := &fakeVolumeMounter{mountOneURI: "nbd:unix:/tmp/test.sock"}
	m := NewManagerWithDeps(Deps{VolumeMounter: mounter, VolumeStateUpdater: stateUpdater})
	m.Insert(&VM{
		ID: "i-1", Status: StateRunning, Instance: &ec2.Instance{},
		QMPClient: qmpClient, AccountID: stateAccountID,
	})

	_, err := m.AttachVolume(t.Context(), "i-1", "vol-1", "/dev/sdf")
	require.Error(t, err)

	calls := stateUpdater.snapshot()
	require.Len(t, calls, 2)
	for i, call := range calls {
		assert.Equalf(t, stateAccountID, call.AccountID,
			"call %d (%s) must be keyed on the instance's own account", i, call.State)
	}
	assert.Equal(t, "available", calls[1].State)
}

// TestDetachVolume_KeysStateOnTheInstanceAccount covers vm/volumes.go:444.
// Detach only logs an UpdateVolumeState failure, so a wrong account here leaves
// the document reading in-use with no disk in the guest and no error anywhere.
func TestDetachVolume_KeysStateOnTheInstanceAccount(t *testing.T) {
	qmpClient, cancel := newMockQMPClientWithEvents(t, func(cmd qmp.QMPCommand, srv *mockQMPServer) map[string]any {
		return nil
	})
	defer cancel()

	stateUpdater := &fakeVolumeStateUpdater{}
	mounter := &fakeVolumeMounter{}
	m := NewManagerWithDeps(Deps{
		VolumeMounter: mounter, VolumeStateUpdater: stateUpdater,
		DeviceDeletedTimeout: 30 * time.Millisecond,
	})
	m.Insert(&VM{
		ID: "i-1", Status: StateRunning, Instance: &ec2.Instance{},
		QMPClient: qmpClient, AccountID: stateAccountID,
		EBSRequests: types.EBSRequests{
			Requests: []types.EBSRequest{{Name: "vol-1", DeviceName: "/dev/sdf"}},
		},
	})

	_, err := m.DetachVolume(t.Context(), "i-1", "vol-1", "", false)
	require.NoError(t, err)

	calls := stateUpdater.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, stateAccountID, calls[0].AccountID,
		"the available write must be keyed on the instance's own account")
	assert.Equal(t, "available", calls[0].State)
}

// TestAttachVolume_StateUpdateErrorSurfacesTheAccount pins that a key-builder
// rejection is not swallowed: an instance whose AccountID cannot key a document
// must fail the attach rather than proceed to device_add.
func TestAttachVolume_StateUpdateErrorSurfacesTheAccount(t *testing.T) {
	recorder := &qmpRecorder{}
	qmpClient, cancel := newMockQMPClient(t, func(cmd qmp.QMPCommand) map[string]any {
		recorder.record(cmd)
		return nil
	})
	defer cancel()

	keyErr := errors.New(`invalid EBS metadata account ID ""`)
	stateUpdater := &fakeVolumeStateUpdater{err: keyErr}
	mounter := &fakeVolumeMounter{mountOneURI: "nbd:unix:/tmp/test.sock"}
	m := NewManagerWithDeps(Deps{VolumeMounter: mounter, VolumeStateUpdater: stateUpdater})
	m.Insert(&VM{ID: "i-1", Status: StateRunning, Instance: &ec2.Instance{}, QMPClient: qmpClient})

	_, err := m.AttachVolume(t.Context(), "i-1", "vol-1", "/dev/sdf")
	require.Error(t, err)
	assert.ErrorIs(t, err, keyErr)
	assert.NotContains(t, recorder.executes(), "device_add",
		"an unkeyable account must not reach the guest")

	calls := stateUpdater.snapshot()
	require.Len(t, calls, 1)
	assert.Empty(t, calls[0].AccountID, "the untenanted instance's account is what reached the store")
}
