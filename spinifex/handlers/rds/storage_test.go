package handlers_rds

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeVolumeResizer is the volume store as a grow reads and writes it, and it
// enforces the store's own grow-only rule so an idempotence bug surfaces here
// rather than in production.
type fakeVolumeResizer struct {
	sizes    map[string]int64
	modified []*ec2.ModifyVolumeInput

	describeErr error
	modifyErr   error
	// missing makes the volume read as gone, which is the state a grow must not
	// interpret as "smaller than the target".
	missing bool

	// onModify runs inside the resize, so a test can observe what else has and
	// has not happened by the time the volume is taken.
	onModify func()
}

var _ volumeResizer = (*fakeVolumeResizer)(nil)

func newFakeVolumeResizer(volumeID string, sizeGiB int64) *fakeVolumeResizer {
	return &fakeVolumeResizer{sizes: map[string]int64{volumeID: sizeGiB}}
}

func (f *fakeVolumeResizer) DescribeVolumes(_ context.Context, in *ec2.DescribeVolumesInput, _ string) (*ec2.DescribeVolumesOutput, error) {
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	out := &ec2.DescribeVolumesOutput{}
	if f.missing {
		return out, nil
	}
	for _, id := range aws.StringValueSlice(in.VolumeIds) {
		size, ok := f.sizes[id]
		if !ok {
			continue
		}
		out.Volumes = append(out.Volumes, &ec2.Volume{VolumeId: aws.String(id), Size: aws.Int64(size)})
	}
	return out, nil
}

func (f *fakeVolumeResizer) ModifyVolume(_ context.Context, in *ec2.ModifyVolumeInput, _ string) (*ec2.ModifyVolumeOutput, error) {
	if f.onModify != nil {
		f.onModify()
	}
	if f.modifyErr != nil {
		return nil, f.modifyErr
	}
	id, size := aws.StringValue(in.VolumeId), aws.Int64Value(in.Size)
	if size < f.sizes[id] {
		return nil, fmt.Errorf("volume %s cannot be shrunk from %d to %d", id, f.sizes[id], size)
	}
	f.modified = append(f.modified, in)
	f.sizes[id] = size
	return &ec2.ModifyVolumeOutput{}, nil
}

// The whole rule set a resize is checked against, ahead of any state change.
func TestValidateStorageGrow(t *testing.T) {
	cases := []struct {
		name              string
		current, request  int64
		wantErrContaining string
	}{
		{name: "grows", current: 20, request: 50},
		{name: "repeats the current size", current: 20, request: 20},
		{name: "shrinks", current: 100, request: 50, wantErrContaining: awserrors.ErrorInvalidParameterCombination},
		{name: "below the floor", current: 20, request: 5, wantErrContaining: awserrors.ErrorInvalidParameterValue},
		{name: "above the ceiling", current: 20, request: maxAllocatedStorageGiB + 1, wantErrContaining: awserrors.ErrorInvalidParameterValue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateStorageGrow(tc.current, tc.request)
			if tc.wantErrContaining == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErrContaining)
		})
	}
}

// The volume store is grow-only, so a resumed grow that re-issued the modify
// would be rejected as a shrink. It reads the volume first instead.
func TestGrowDataVolume_SkipsAVolumeAlreadyAtTheTarget(t *testing.T) {
	t.Parallel()
	h := newModifyHarness(t)
	h.storage.sizes[testDataVolume] = 50

	require.NoError(t, h.svc.growDataVolume(t.Context(), testDataVolume, 50))
	assert.Empty(t, h.storage.modified, "a volume already at the target is not modified again")
}

// A volume the store cannot describe must not read as zero: zero is smaller
// than every target, so the grow would go ahead against a volume that is gone.
func TestGrowDataVolume_FailsWhenTheVolumeIsGone(t *testing.T) {
	t.Parallel()
	h := newModifyHarness(t)
	h.storage.missing = true

	err := h.svc.growDataVolume(t.Context(), testDataVolume, 50)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no longer exists")
	assert.Empty(t, h.storage.modified)
}

func TestGrowDataVolume_FailsWithoutAVolume(t *testing.T) {
	t.Parallel()
	h := newModifyHarness(t)

	require.Error(t, h.svc.growDataVolume(t.Context(), "", 50))
}

// ModifyVolume refuses a volume a running VM holds, so the order is the
// whole mechanism — engine down, VM down, grow, VM back.
func TestGrowInstanceStorage_StopsTheEngineAndTheVMBeforeGrowing(t *testing.T) {
	t.Parallel()
	h := newModifyHarness(t)
	rec := modifiableRecord()

	var vmStoppedAtGrow bool
	h.storage.onModify = func() {
		vmStoppedAtGrow = assert.ObjectsAreEqual([]string{"stop:" + testInstance}, h.cmdr.calls)
	}

	require.NoError(t, h.svc.growInstanceStorage(t.Context(), testAccountID, &rec, 50))

	assert.True(t, vmStoppedAtGrow, "the volume was taken while the VM still held it")
	assert.Equal(t, []string{"stop:" + testInstance, "start:" + testInstance}, h.cmdr.calls)
	assert.Equal(t, int64(50), h.storage.sizes[testDataVolume])

	issued := h.agent.received()
	require.Len(t, issued, 1)
	assert.Equal(t, CommandStopEngine, issued[0].Type)
}

// The stop command is accepted seconds before the volume detaches, and the
// store refuses a resize until it has. The grow holds off until the fleet
// reports the VM down rather than resizing on the accepted command.
func TestGrowInstanceStorage_WaitsForTheDetachBeforeGrowing(t *testing.T) {
	t.Parallel()
	h := newModifyHarness(t)
	h.vmState.detachReads = 1
	rec := modifiableRecord()

	var vmDrainingAtGrow bool
	h.storage.onModify = func() { vmDrainingAtGrow = h.vmState.detachReads > 0 }

	require.NoError(t, h.svc.growInstanceStorage(t.Context(), testAccountID, &rec, 50))

	assert.False(t, vmDrainingAtGrow, "the volume was taken while the fleet still reported the VM stopping")
	assert.Len(t, h.vmState.calls, 2, "the first reading still had the VM stopping")
	assert.Equal(t, int64(50), h.storage.sizes[testDataVolume])
}

// A wedged agent must not block a grow: the engine stop is a courtesy, and the
// VM stop that follows is what actually releases the volume.
func TestGrowInstanceStorage_ProceedsWhenTheEngineWillNotStop(t *testing.T) {
	t.Parallel()
	h := newModifyHarnessWithAgent(t, true)
	rec := modifiableRecord()

	require.NoError(t, h.svc.growInstanceStorage(t.Context(), testAccountID, &rec, 50))
	assert.Equal(t, int64(50), h.storage.sizes[testDataVolume])
}

// A rejected grow must not turn a failed modification into an outage. The VM
// restarts on its unchanged volume before the original error is returned.
func TestGrowInstanceStorage_RestartsTheVMWhenTheGrowFails(t *testing.T) {
	t.Parallel()
	h := newModifyHarness(t)
	h.storage.modifyErr = errors.New("the volume store is unavailable")
	rec := modifiableRecord()

	err := h.svc.growInstanceStorage(t.Context(), testAccountID, &rec, 50)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the volume store is unavailable")
	assert.Equal(t, []string{"stop:" + testInstance, "start:" + testInstance}, h.cmdr.calls)
}

// Both failures matter when the grow and its recovery fail: the first explains
// the unchanged storage and the second explains the continuing outage.
func TestGrowInstanceStorage_ReportsTheGrowAndRestartFailures(t *testing.T) {
	t.Parallel()
	h := newModifyHarness(t)
	h.storage.modifyErr = errors.New("the volume store is unavailable")
	h.storage.onModify = func() { h.cmdr.err = errors.New("the node did not answer") }
	rec := modifiableRecord()

	err := h.svc.growInstanceStorage(t.Context(), testAccountID, &rec, 50)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the volume store is unavailable")
	assert.Contains(t, err.Error(), "restart the DB VM after the storage grow failed")
	assert.Contains(t, err.Error(), "the node did not answer")
	assert.Equal(t, []string{"stop:" + testInstance, "start:" + testInstance}, h.cmdr.calls)
}

// Recovery belongs to the new lease holder after a takeover. The stale holder
// must not restart the VM while its replacement may be retrying the grow.
func TestGrowInstanceStorage_DoesNotRestartAfterLosingTheModifyLease(t *testing.T) {
	t.Parallel()
	h := newModifyHarness(t)
	ctx, cancel := context.WithCancelCause(t.Context())
	h.storage.modifyErr = errors.New("the volume store is unavailable")
	h.storage.onModify = func() { cancel(errModifyLeaseLost) }
	rec := modifiableRecord()

	err := h.svc.growInstanceStorage(ctx, testAccountID, &rec, 50)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the volume store is unavailable")
	assert.Equal(t, []string{"stop:" + testInstance}, h.cmdr.calls)
}

// Stopping the VM is what makes the volume available, so a stop that did not
// happen must not be followed by a resize that would be rejected — or worse,
// accepted against a volume a live guest is writing to.
func TestGrowInstanceStorage_FailsWhenTheVMWillNotStop(t *testing.T) {
	t.Parallel()
	h := newModifyHarness(t)
	h.cmdr.err = errors.New("the node did not answer")
	rec := modifiableRecord()

	err := h.svc.growInstanceStorage(t.Context(), testAccountID, &rec, 50)
	require.Error(t, err)
	assert.Empty(t, h.storage.modified)
}
